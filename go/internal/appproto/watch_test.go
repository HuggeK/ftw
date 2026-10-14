package appproto

import (
	"slices"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/control"
)

// The phone that added the watch in these tests, and the watch itself.
const (
	watchParentSubject = apiauth.KindApp + ":aBcD1234"
	watchSubject       = apiauth.KindApp + ":wAtCh123"
)

// watchCaller is the caller a watch's session is built with, as the uplink
// builds it: the role's scopes that its owner phone also holds.
func watchCaller() apiauth.Caller {
	return apiauth.Caller{
		Subject: watchSubject,
		Kind:    apiauth.KindApp,
		Role:    apiauth.RoleWatch,
		Scopes:  ScopesForGrant(apiauth.RoleWatch, apiauth.RoleOwner),
		Epoch:   1,
	}
}

func newWatchGrants() *fakeGrants {
	return &fakeGrants{
		role: apiauth.RoleWatch, epoch: 1, parentRole: apiauth.RoleOwner, root: watchParentSubject,
	}
}

// A watch reads, sees the plan and alerts, and charges. Nothing else, and
// never more than its phone holds now.
func TestAWatchHoldsOnlyWhatItsPhoneHolds(t *testing.T) {
	underOwner := GrantScopes(apiauth.RoleWatch, apiauth.RoleOwner)
	if !slices.Equal(underOwner, apiauth.RoleScopes[apiauth.RoleWatch]) {
		t.Fatalf("under an owner a watch holds %v, want its role's %v", underOwner, apiauth.RoleScopes[apiauth.RoleWatch])
	}
	for _, never := range []string{ScopeModeWrite, ScopeMembersRead, ScopeMembersWrite, ScopeHistoryRead} {
		if slices.Contains(underOwner, never) {
			t.Fatalf("a watch holds %s", never)
		}
	}

	underViewer := GrantScopes(apiauth.RoleWatch, apiauth.RoleViewer)
	if !slices.Equal(underViewer, []string{ScopeLiveRead}) {
		t.Fatalf("under a viewer a watch holds %v, want only %s", underViewer, ScopeLiveRead)
	}
	if got := GrantScopes(apiauth.RoleWatch, ""); len(got) != 0 {
		t.Fatalf("a watch with no phone holds %v", got)
	}
	if got := GrantScopes(apiauth.RoleOwner, ""); !slices.Equal(got, apiauth.RoleScopes[apiauth.RoleOwner]) {
		t.Fatalf("an owner phone holds %v", got)
	}
}

// A watch may charge and may not change the site mode.
func TestAWatchChargesButCannotSetTheMode(t *testing.T) {
	h, lp, box, rec, _ := newEVRig(t)
	h.cfg.Caller = watchCaller()
	h.cfg.Grants = newWatchGrants()
	subscribe(t, h, rec)

	deliver(t, h, MsgCmd, nil, cmdSetMode(string(control.ModeCharge), 7, 200_000, nil))
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrScopeDenied {
		t.Fatalf("site.mode.set from a watch = %+v, want rejected/%s", res, ErrScopeDenied)
	}
	if box.setModeCalls != 0 {
		t.Fatal("a watch changed the site mode")
	}
	rec.reset()

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-watch-hold", holdArgs(), 7, 200_000))
	if res := body[CmdResult](t, rec.only(t, MsgCmdResult)); res.State != CmdApplied {
		t.Fatalf("a watch's charge ended %q (%+v)", res.State, res.Error)
	}
	if lp.holdCalls != 1 {
		t.Fatalf("the charger saw %d holds", lp.holdCalls)
	}
}

// Demoting the phone narrows its watch at the next command, with no new
// session: the watch never does more than its phone can do now.
func TestDemotingThePhoneNarrowsItsWatchAtOnce(t *testing.T) {
	h, lp, _, rec, _ := newEVRig(t)
	grants := newWatchGrants()
	h.cfg.Caller = watchCaller()
	h.cfg.Grants = grants
	subscribe(t, h, rec)

	grants.mu.Lock()
	grants.parentRole = apiauth.RoleViewer
	grants.epoch++
	grants.mu.Unlock()

	deliver(t, h, MsgCmd, nil, cmdLoadpoint(OpLoadpointHold, "cmd-after-demotion", holdArgs(), 7, 200_000))
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrScopeDenied {
		t.Fatalf("result = %+v, want rejected/%s once the phone is a viewer", res, ErrScopeDenied)
	}
	if lp.controllerCalls() != 0 {
		t.Fatal("a watch charged after its phone lost the right to")
	}
}

// A watch spends its phone's command ids. One intent sent by the watch and
// again by its phone acts once.
func TestAWatchAndItsPhoneShareCommandIds(t *testing.T) {
	h, lp, box, rec, clock := newEVRig(t)
	h.cfg.Caller = watchCaller()
	h.cfg.Grants = newWatchGrants()
	subscribe(t, h, rec)

	cmd := cmdLoadpoint(OpLoadpointHold, "cmd-both-paths", holdArgs(), 7, 200_000)
	deliver(t, h, MsgCmd, nil, cmd)
	first := body[CmdAck](t, rec.only(t, MsgCmdAck))

	phone := ownerCaller()
	phone.Subject = watchParentSubject
	next, nextRec := sessionOf(t, h, phone, box, clock)
	deliver(t, next, MsgCmd, nil, cmd)

	if lp.holdCalls != 1 {
		t.Fatalf("the charger saw %d holds for one intent", lp.holdCalls)
	}
	if got := body[CmdAck](t, nextRec.only(t, MsgCmdAck)); got.LeaseID != first.LeaseID {
		t.Fatalf("the phone got lease %q, the watch %q", got.LeaseID, first.LeaseID)
	}

	// Another phone's same id is still its own intent.
	stranger := ownerCaller()
	stranger.Subject = apiauth.KindApp + ":qRsT5678"
	other, otherRec := sessionOf(t, h, stranger, box, clock)
	deliver(t, other, MsgCmd, nil, cmd)
	if res := body[CmdResult](t, otherRec.only(t, MsgCmdResult)); res.State != CmdApplied || lp.holdCalls != 2 {
		t.Fatalf("another phone's command ended %q after %d holds", res.State, lp.holdCalls)
	}
}

// hello_ok tells the watch what it holds, so it draws no button its phone has
// lost.
func TestAWatchsHelloNamesItsRoleAndScopes(t *testing.T) {
	h, _, rec, _ := newRig(t)
	h.cfg.Caller = watchCaller()
	h.cfg.Grants = newWatchGrants()

	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: ProtoMin, Max: ProtoMax}})
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Role != apiauth.RoleWatch {
		t.Fatalf("hello_ok role = %q", ok.Role)
	}
	want := append([]string(nil), apiauth.RoleScopes[apiauth.RoleWatch]...)
	slices.Sort(want)
	if !slices.Equal(ok.Scopes, want) {
		t.Fatalf("hello_ok scopes = %v, want %v", ok.Scopes, want)
	}
	if slices.Contains(ok.Caps, CapCmdExchange) {
		t.Fatal("hello_ok says cmd.exchange for a box that was not given it")
	}
}
