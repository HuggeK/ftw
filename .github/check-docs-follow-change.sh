#!/usr/bin/env bash
# Reports where the documentation stopped following the code -- here, and on the
# website, which is the only description of FTW that anyone who has not cloned
# this repository ever reads.
#
# Three things drift, in ascending order of how quietly they manage it.
#
# The website deep-links in here: a README anchor behind each install button,
# docs/ pages, the raw install.sh URL the front page tells people to pipe into
# bash. Nothing in this repository knew those links existed, so they rotted --
# when this check was written the four install buttons pointed at
# #option-a-raspberry-pi-sd-card-image through #option-d-build-from-source and
# the closing "Get started" button at #quick-start, five headings this README
# had not had for months. GitHub drops a fragment it cannot find instead of
# erroring, so each of those clicks left a visitor at the top of a long README
# with no sign that anything had gone wrong. "Browse drivers" opened a drivers/
# tree that no longer holds driver source.
#
# The install command on the front page is a copy of the one in this README,
# not something generated from it, and two copies of one command is one copy too
# many.
#
# And a change can be documented perfectly in here and still reach nobody. A
# capability described only under docs/ is a capability for people who have
# already cloned the repository, which is nobody who is still deciding whether
# to. So when a change carries a changeset -- this repository's own test for
# "a user can see this" -- the check names the documents that describe what the
# change touched, says whether any of them moved with it, and lists the sections
# of the website that currently make promises about that area.
#
# It never fails the merge. Some of what it reports is a judgement call about a
# repository one over, and a check that blocks on a judgement call gets
# satisfied rather than read. Put "no-website-change" on a line of its own in
# the pull request description when a change genuinely does not reach the site,
# and that section goes quiet with the decision recorded where review can see
# it.
#
# Run it the way CI does, or against a local checkout of the site:
#
#   .github/check-docs-follow-change.sh
#   WEB_SITE_FILE=../ftw-web/index.html .github/check-docs-follow-change.sh
#
# Exit codes: 0 nothing to report, 1 something to read, 2 the check could not
# run -- a network failure must not read as "the documentation is fine".
set -euo pipefail

SITE_URL="${WEB_SITE_URL:-https://raw.githubusercontent.com/srcfl/ftw-web/main/index.html}"
SITE_FILE="${WEB_SITE_FILE:-}"
BASE_SHA="${DOCS_FOLLOW_BASE_SHA:-}"
PR_BODY="${DOCS_FOLLOW_PR_BODY:-}"
DEFAULT_REF="master"
WEB_REPO="https://github.com/srcfl/ftw-web"

# Prose that describes the project to a person, as opposed to prose that
# describes a contract to a machine. These are the files a reader gets sent to.
DOC_PATHSPEC=(README.md SUPPORT.md CONTRIBUTING.md AGENTS.md docs/)

cd "$(git rev-parse --show-toplevel)"

site="$(mktemp)"
trap 'rm -f "${site}"' EXIT

if [ -n "${SITE_FILE}" ]; then
  if [ ! -f "${SITE_FILE}" ]; then
    echo "site source not found: ${SITE_FILE}" >&2
    exit 2
  fi
  cp "${SITE_FILE}" "${site}"
  source_label="${SITE_FILE}"
else
  if ! curl -fsSL --retry 3 --max-time 30 -o "${site}" "${SITE_URL}"; then
    echo "could not fetch the website source from ${SITE_URL}" >&2
    echo "the website was NOT compared against this checkout" >&2
    exit 2
  fi
  source_label="${SITE_URL}"
fi

# ---------------------------------------------------------------------------
# What this change is
# ---------------------------------------------------------------------------

base=""
changed=""
removed=""
if [ -n "${BASE_SHA}" ] && git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  base="${BASE_SHA}"
  changed="$(git diff --name-only "${BASE_SHA}"...HEAD || true)"
  # A rename removes the old path, and the old path is the one the site links.
  removed="$(git diff --name-status --diff-filter=DR "${BASE_SHA}"...HEAD |
    awk '{print $2}' || true)"
fi

# This repository already has a test for "a user can see this": the change needs
# a changeset. Reusing it means this check inherits that judgement instead of
# inventing a second, competing one -- and `changeset add --empty` opts out of
# both at once.
user_visible=""
changeset_summary=""
if [ -n "${changed}" ]; then
  while IFS= read -r file; do
    case "${file}" in
      .changeset/README.md) continue ;;
      .changeset/*.md) ;;
      *) continue ;;
    esac
    [ -f "${file}" ] || continue
    # An empty changeset is frontmatter with no package line.
    if grep -qE '^"[^"]+": *(major|minor|patch) *$' "${file}"; then
      user_visible="yes"
      if [ -z "${changeset_summary}" ]; then
        # The summary is the first prose line after the closing --- of the
        # frontmatter, which is the second --- in the file.
        changeset_summary="$(awk '
          /^---[[:space:]]*$/ { fences++; next }
          fences >= 2 && NF { print; exit }' "${file}")"
      fi
    fi
  done <<<"${changed}"
fi

# The marker has to sit on a line of its own, optionally bulleted, ticked or
# followed by a reason. A pull request that merely mentions "no-website-change"
# mid-sentence -- this check's own description does -- is discussing the escape
# hatch, not taking it.
website_opt_out=""
if printf '%s\n' "${PR_BODY}" |
  grep -qiE '^[[:space:]]*([-*+][[:space:]]*)?(\[[ xX]\][[:space:]]*)?`?no-website-change`?[[:space:]]*([:-][^|]*)?$'; then
  website_opt_out="yes"
fi

# ---------------------------------------------------------------------------
# Reading the checkout and the site
# ---------------------------------------------------------------------------

# GitHub's heading slug, close enough for headings written in English prose:
# lowercase, drop everything that is not a letter, digit, space, underscore or
# hyphen, then spaces to hyphens.
slugs() {
  grep -E '^ {0,3}#{1,6}[[:space:]]+' |
    sed -E 's/^ {0,3}#+[[:space:]]+//; s/[[:space:]]+$//' |
    tr '[:upper:]' '[:lower:]' |
    sed -E 's/[^a-z0-9 _-]//g; s/ +/-/g'
}

# Every lookup takes a revision so a link can be resolved twice: once against
# the checkout, to say what is broken, and once against the base commit, to say
# whether this change is what broke it. An empty revision means the checkout.
#
# A file GitHub can render is a file git knows about, which is why the checkout
# side asks git rather than the filesystem: drivers/*.lua is present in a
# working copy after `make drivers` and 404s on github.com.
file_at() { # rev path
  if [ -z "$1" ]; then
    git ls-files --error-unmatch -- "$2" >/dev/null 2>&1 && [ -f "$2" ]
  else
    git cat-file -e "$1:$2" 2>/dev/null
  fi
}

dir_holds_files_at() { # rev path
  if [ -z "$1" ]; then
    [ -d "$2" ] && [ -n "$(git ls-files -- "$2" | head -n 1)" ]
  else
    [ -n "$(git ls-tree -r --name-only "$1" -- "$2" 2>/dev/null | head -n 1)" ]
  fi
}

anchors_at() { # rev path
  if [ -z "$1" ]; then
    slugs <"$2"
  else
    git show "$1:$2" 2>/dev/null | slugs
  fi
}

has_anchor_at() { anchors_at "$1" "$2" | grep -qxF -- "$3"; }

resolves_at() { # rev kind path anchor
  local rev="$1" kind="$2" path="$3" anchor="$4"
  if [ "${kind}" = "tree" ]; then
    dir_holds_files_at "${rev}" "${path}"
    return
  fi
  file_at "${rev}" "${path}" || return 1
  [ -z "${anchor}" ] && return 0
  case "${path}" in
    *.md) has_anchor_at "${rev}" "${path}" "${anchor}" ;;
    *) return 0 ;;
  esac
}

report() { # url reason detail
  local entry
  entry="  $1"$'\n'"      $2"
  [ -n "$3" ] && entry="${entry}"$'\n'"      $3"
  printf '%s\n' "${entry}"
}

# ---------------------------------------------------------------------------
# 1. The website's links into this repository
# ---------------------------------------------------------------------------

broken_here=""
broken_already=""
hollowed=""
skipped_refs=""
checked=0

removals_under() {
  [ -z "${removed}" ] && return 0
  printf '%s\n' "${removed}" |
    grep -cE "^$(printf '%s' "$1" | sed -E 's/[][\.^$*+?(){}|/]/\\&/g')(/|$)" || true
}

links="$(grep -oE 'https://(github\.com|raw\.githubusercontent\.com)/srcfl/ftw[^"'"'"'[:space:]<>)]*' "${site}" |
  sed -E 's/[.,;:]+$//' | sort -u || true)"

while IFS= read -r url; do
  [ -z "${url}" ] && continue

  ref=""
  path=""
  anchor=""
  kind=""

  case "${url}" in
    # Another repository under the same owner -- srcfl/ftw-web, srcfl/ftwdb.
    # Its paths cannot be resolved from this checkout.
    https://github.com/srcfl/ftw-* | https://raw.githubusercontent.com/srcfl/ftw-*) continue ;;

    https://github.com/srcfl/ftw | https://github.com/srcfl/ftw/)
      checked=$((checked + 1))
      continue
      ;;

    https://github.com/srcfl/ftw#*)
      kind="anchor"
      path="README.md"
      anchor="${url#*#}"
      ;;

    https://github.com/srcfl/ftw/blob/*)
      kind="blob"
      rest="${url#https://github.com/srcfl/ftw/blob/}"
      ref="${rest%%/*}"
      path="${rest#*/}"
      case "${path}" in *#*)
        anchor="${path#*#}"
        path="${path%%#*}"
        ;;
      esac
      ;;

    https://github.com/srcfl/ftw/tree/*)
      kind="tree"
      rest="${url#https://github.com/srcfl/ftw/tree/}"
      ref="${rest%%/*}"
      path="${rest#*/}"
      path="${path%%#*}"
      ;;

    https://raw.githubusercontent.com/srcfl/ftw/*)
      kind="raw"
      rest="${url#https://raw.githubusercontent.com/srcfl/ftw/}"
      ref="${rest%%/*}"
      path="${rest#*/}"
      ;;

    # GitHub's own pages. They exist as long as the repository does.
    https://github.com/srcfl/ftw/issues* | https://github.com/srcfl/ftw/pulls* | \
      https://github.com/srcfl/ftw/releases* | https://github.com/srcfl/ftw/discussions* | \
      https://github.com/srcfl/ftw/actions* | https://github.com/srcfl/ftw/commits* | \
      https://github.com/srcfl/ftw/compare* | https://github.com/srcfl/ftw/security* | \
      https://github.com/srcfl/ftw/wiki*)
      checked=$((checked + 1))
      continue
      ;;

    *)
      checked=$((checked + 1))
      continue
      ;;
  esac

  checked=$((checked + 1))

  # A link into a branch or tag this repository does not publish cannot be
  # checked against the checkout, and is usually a mistake on its own.
  if [ -n "${ref}" ] && [ "${ref}" != "${DEFAULT_REF}" ]; then
    skipped_refs="${skipped_refs}$(report "${url}" \
      "links ref \"${ref}\"; this repository's default branch is ${DEFAULT_REF}" "")"$'\n'
    continue
  fi

  problem=""
  detail=""

  case "${kind}" in
    tree)
      if [ ! -d "${path}" ]; then
        problem="no directory ${path}/ in this repository"
      elif ! dir_holds_files_at "" "${path}"; then
        problem="${path}/ holds no files tracked by git, so GitHub shows it as empty"
      fi
      ;;
    *)
      if [ ! -e "${path}" ]; then
        problem="no file ${path} in this repository"
      elif ! file_at "" "${path}"; then
        problem="${path} is not tracked by git, so this link 404s"
      elif [ -n "${anchor}" ]; then
        case "${path}" in
          *.md)
            if ! has_anchor_at "" "${path}" "${anchor}"; then
              problem="${path} has no heading that slugs to \"${anchor}\""
              detail="it now has: $(anchors_at "" "${path}" | paste -sd' ' -)"
            fi
            ;;
        esac
      fi
      ;;
  esac

  if [ -n "${problem}" ]; then
    # Blame is worth getting right. A link that was already broken when this
    # branch started is somebody else's cleanup, and reporting it as this
    # change's fault is how a check earns the reputation that gets it ignored.
    if [ -n "${base}" ] && resolves_at "${base}" "${kind}" "${path}" "${anchor}"; then
      broken_here="${broken_here}$(report "${url}" "${problem}" "${detail}")"$'\n'
    else
      broken_already="${broken_already}$(report "${url}" "${problem}" "${detail}")"$'\n'
    fi
    continue
  fi

  # The link still resolves, but this change emptied out what the visitor came
  # for. This is how the driver move read from in here: drivers/ stayed, its 37
  # drivers did not.
  if [ "${kind}" = "tree" ]; then
    gone="$(removals_under "${path}")"
    if [ "${gone:-0}" -gt 0 ]; then
      hollowed="${hollowed}$(report "${url}" \
        "still resolves, but this change removes ${gone} tracked file(s) under ${path}/" "")"$'\n'
    fi
  fi
done <<<"${links}"

# ---------------------------------------------------------------------------
# 2. The install command, which exists twice
# ---------------------------------------------------------------------------

install_command_in() { # file
  grep -ohE 'curl -fsSL https://raw\.githubusercontent\.com/srcfl/ftw/[^ ]+ \| bash' "$1" |
    head -n 1 || true
}

readme_install="$(install_command_in README.md)"
site_install="$(install_command_in "${site}")"
install_drift=""

if [ -n "${site_install}" ] && [ -n "${readme_install}" ] &&
  [ "${site_install}" != "${readme_install}" ]; then
  install_drift="  the site tells people to run:"$'\n'"      ${site_install}"$'\n'
  install_drift="${install_drift}  this README publishes:"$'\n'"      ${readme_install}"$'\n'
elif [ -n "${site_install}" ] && [ -z "${readme_install}" ]; then
  install_drift="  the site tells people to run:"$'\n'"      ${site_install}"$'\n'
  install_drift="${install_drift}  this README no longer publishes an install command in that form"$'\n'
fi

# ---------------------------------------------------------------------------
# 3. Documents in here that describe what this change touched
# ---------------------------------------------------------------------------

is_doc() { # path
  case "$1" in
    docs/*) return 0 ;;
    README.md | SUPPORT.md | CONTRIBUTING.md | AGENTS.md) return 0 ;;
    *) return 1 ;;
  esac
}

docs_touched=""
search_terms=""
if [ -n "${changed}" ]; then
  while IFS= read -r file; do
    [ -z "${file}" ] && continue
    if is_doc "${file}"; then
      docs_touched="${docs_touched}${file}"$'\n'
      continue
    fi
    case "${file}" in
      .changeset/* | .github/* | CHANGELOG.md | package-lock.json) continue ;;
    esac
    search_terms="${search_terms}${file}"$'\n'
    dir="$(dirname -- "${file}")"
    [ "${dir}" != "." ] && search_terms="${search_terms}${dir}/"$'\n'
  done <<<"${changed}"
fi
search_terms="$(printf '%s' "${search_terms}" | sed '/^$/d' | sort -u | head -n 25)"

# A document that names the path you changed is a document that was describing
# it. Deriving that beats maintaining a path-to-document table that nobody
# updates when a package gets renamed.
describing_docs=""
if [ -n "${search_terms}" ]; then
  while IFS= read -r term; do
    [ -z "${term}" ] && continue
    hits="$(git grep -l -F -e "${term}" -- "${DOC_PATHSPEC[@]}" 2>/dev/null || true)"
    while IFS= read -r hit; do
      [ -z "${hit}" ] && continue
      describing_docs="${describing_docs}${hit}	${term}"$'\n'
    done <<<"${hits}"
  done <<<"${search_terms}"
fi
describing_docs="$(printf '%s' "${describing_docs}" | sed '/^$/d' | sort -u || true)"

# ---------------------------------------------------------------------------
# 4. What the website currently promises
# ---------------------------------------------------------------------------

# Read the site's own structure rather than hardcoding it: each section and the
# heading it leads with.
site_sections() {
  awk '
    match($0, /<section[^>]*id="[^"]+"/) {
      chunk = substr($0, RSTART, RLENGTH)
      match(chunk, /id="[^"]+"/)
      pending = substr(chunk, RSTART + 4, RLENGTH - 5)
      next
    }
    pending != "" && /<h[12][^>]*>/ {
      line = $0
      gsub(/<[^>]*>/, "", line)
      gsub(/^[ \t]+|[ \t]+$/, "", line)
      if (line != "") { printf "  #%-15s %s\n", pending, line; pending = "" }
    }
  ' "${site}"
}

readme_doc_list() {
  awk '
    /^## Documentation/ { inside = 1; next }
    inside && /^## / { exit }
    inside && match($0, /\]\([^)]+\)/) {
      link = substr($0, RSTART + 2, RLENGTH - 3)
      if (link !~ /^http/) print "  " link
    }
  ' README.md
}

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------

echo "documentation follows the change"
echo "  website source: ${source_label}"
echo "  website links into this repository: ${checked} checked"
if [ -n "${user_visible}" ]; then
  echo "  this change carries a changeset, so this repository calls it user-visible:"
  [ -n "${changeset_summary}" ] && echo "      ${changeset_summary}"
fi

status=0

if [ -n "${broken_here}" ]; then
  status=1
  echo
  echo "website links this change breaks:"
  printf '%s' "${broken_here}"
fi

if [ -n "${broken_already}" ]; then
  status=1
  echo
  echo "website links already broken before this change:"
  printf '%s' "${broken_already}"
fi

if [ -n "${skipped_refs}" ]; then
  status=1
  echo
  echo "website links pointing at a ref this repository does not publish:"
  printf '%s' "${skipped_refs}"
fi

if [ -n "${hollowed}" ]; then
  status=1
  echo
  echo "website links that resolve, but no longer hold what they send people to see:"
  printf '%s' "${hollowed}"
fi

if [ -n "${install_drift}" ]; then
  status=1
  echo
  echo "the install command exists in two places and they disagree:"
  printf '%s' "${install_drift}"
fi

if [ -n "${user_visible}" ]; then
  if [ -n "${docs_touched}" ]; then
    echo
    echo "documentation that moved with this change:"
    printf '%s' "${docs_touched}" | sed 's/^/  /'
  elif [ -n "${describing_docs}" ]; then
    status=1
    echo
    echo "no documentation moved with this change, and these documents describe what it touched:"
    printf '%s\n' "${describing_docs}" |
      awk -F'\t' '
        { if (!($1 in seen)) { seen[$1] = 1; order[++n] = $1 }
          terms[$1] = terms[$1] " " $2 }
        END {
          for (i = 1; i <= n; i++) {
            doc = order[i]
            count = split(terms[doc], t, " ")
            out = ""
            for (a = 1; a <= count; a++) {
              if (t[a] == "") continue
              # Naming the file is enough; no need to also report the document
              # for naming the directory the file sits in.
              keep = 1
              for (b = 1; b <= count; b++) {
                if (b != a && t[b] != t[a] && index(t[b], t[a]) == 1) { keep = 0; break }
              }
              if (keep) out = out (out == "" ? "" : ", ") t[a]
            }
            print doc "\t" out
          }
        }' |
      sort |
      awk -F'\t' '{ printf "  %s\n      names %s\n", $1, $2 }' |
      head -n 40
  else
    status=1
    echo
    echo "no documentation moved with this change, and nothing under docs/ names the paths it touched."
    echo "the documents a reader gets sent to are:"
    readme_doc_list
  fi

  if [ -n "${website_opt_out}" ]; then
    echo
    echo "website: skipped, the pull request description says no-website-change"
  else
    status=1
    echo
    echo "the website is where this change becomes visible to anyone who has not cloned the"
    echo "repository. It describes FTW in these sections today:"
    site_sections
  fi
fi

if [ "${status}" = "0" ]; then
  echo
  echo "nothing to report: the website's links resolve and no user-visible change is left undescribed"
  exit 0
fi

cat <<MSG

What to do with the above:

1. In this repository — update the documents named above, or say in the pull
   request why what they already say is still true. A document that describes
   the path you changed and did not change with it is the next reader's wrong
   answer.
2. On the website — open a pull request that says what this change means for
   somebody deciding whether to run FTW, not what it does in the code:

       ${WEB_REPO}/blob/main/index.html

   Broken links there are fixed there too; nothing in this repository can do it.
3. If the change genuinely does not reach the website, write "no-website-change"
   in the pull request description. That section then stays quiet, and the
   decision is recorded where review can see it.

None of this fails the merge. A heading this README no longer has is not a
redirect: GitHub drops the fragment and leaves the visitor at the top of a long
page, which is how a broken install button went unnoticed for months.
MSG

exit "${status}"
