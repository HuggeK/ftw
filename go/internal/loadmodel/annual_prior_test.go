package loadmodel

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestAnnualPriorLeavesLearningIntact(t *testing.T) {
	s := NewService(nil, nil, "meter", 5000, 0)
	annual := 8760.0
	s.SetAnnualBase(annual, false)
	m := s.Model()
	mean := 0.0
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for i := 0; i < Buckets; i++ {
		mean += m.PredictNoTemp(start.Add(time.Duration(i)*time.Hour)) / Buckets
	}
	if math.Abs(mean-1000) > 1e-8 || m.Samples != 0 {
		t.Fatalf("annual prior mean=%v samples=%v", mean, m.Samples)
	}
	trained := s.models[ProfileHome]
	for i := range trained.Bucket {
		trained.Bucket[i].Mean = 750
		trained.Bucket[i].Days = MinTrustSamples
		trained.Bucket[i].Samples = 100
	}
	trained.Samples = 1000
	trained.ConfigRevision = "physical-source-1"
	before := *trained
	s.SetAnnualBase(17520, false)
	after := s.Model()
	if !reflect.DeepEqual(before.Bucket, after.Bucket) || before.Samples != after.Samples || before.ConfigRevision != after.ConfigRevision {
		t.Fatal("changing prior changed measured learning")
	}
	if after.PredictNoTemp(start) != before.PredictNoTemp(start) {
		t.Fatal("prior changed a fully learned bucket")
	}
	s.SetAnnualBase(0, false)
	if s.Model().PriorScale != 1 {
		t.Fatal("removing prior did not restore generic estimate")
	}
}

func TestAnnualWholeLoadPriorDoesNotAddLearnedHeatingTwice(t *testing.T) {
	s := NewService(nil, nil, "meter", 5000, 0)
	s.SetAnnualBase(8760, true)
	m := s.models[ProfileHome]
	m.HeatingW_per_degC = 100
	at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if got := m.Predict(at, 0); got != m.prior(m.hourOfWeek(at)) {
		t.Fatalf("unlearned annual whole-load prior added heat again: %v", got)
	}
}

func TestAnnualHeatingPriorFadesWithMeasuredCoverageAndSurvivesReset(t *testing.T) {
	s := NewService(nil, nil, "meter", 5000, 0)
	s.SetAnnualBase(8760, true)
	m := s.models[ProfileHome]
	m.HeatingW_per_degC = 100
	at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	idx := m.hourOfWeek(at)
	m.Bucket[idx].Mean = 700
	m.Bucket[idx].Days = 1
	m.Bucket[idx].LastMs = at.UnixMilli()
	trust := m.Coverage(at)
	want := trust*(700+100*HeatingReferenceC) + (1-trust)*m.prior(idx)
	if got := m.Predict(at, 0); math.Abs(got-want) > 1e-8 {
		t.Fatal("prior and measured heating overlap", got, want)
	}
	m.Bucket[idx].Days = MinTrustSamples
	if got := m.Predict(at, 0); got != 700+100*HeatingReferenceC {
		t.Fatal("prior changed fully learned heat", got)
	}
	scale := m.PriorScale
	s.Reset()
	reset := s.Model()
	if reset.PriorScale != scale || !reset.PriorIncludesHeating || reset.Samples != 0 {
		t.Fatal("reset lost owner estimate or retained learning")
	}
}
