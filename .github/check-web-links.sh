#!/usr/bin/env bash
# Reports links on the website that point into this repository and no longer
# resolve.
#
# https://ftw.sourceful.energy is a separate repository, srcfl/ftw-web, and it
# deep-links in here: a README anchor behind each install button, docs/ pages,
# the raw install.sh URL the front page tells people to pipe into bash, the
# driver directory. Nothing in this repo knows those links exist, so renaming a
# heading breaks the site's install page and nobody learns it from here.
#
# It had already happened when this check was written. The site's four install
# buttons pointed at #option-a-raspberry-pi-sd-card-image through
# #option-d-build-from-source and its closing "Get started" button at
# #quick-start -- five headings this README had not had for months, so every one
# of those clicks quietly landed at the top of the page instead. "Browse
# drivers" opened drivers/, which stopped holding driver source when it moved to
# srcfl/device-drivers.
#
# This does not fail the merge. The fix lives in another repository and often in
# another person's hands, and blocking a driver fix here on a website PR there
# would cost more than the broken link does. The check exists so the person who
# moved the heading hears about it while they still remember where it moved to.
#
# Run it by hand the same way CI does:
#
#   .github/check-web-links.sh                                  # the live site
#   WEB_SITE_FILE=../ftw-web/index.html .github/check-web-links.sh
#
# Exit codes: 0 everything resolves, 1 something is broken, 2 the check could
# not run -- a network failure must not read as "the links are fine".
set -euo pipefail

SITE_URL="${WEB_SITE_URL:-https://raw.githubusercontent.com/srcfl/ftw-web/main/index.html}"
SITE_FILE="${WEB_SITE_FILE:-}"
BASE_SHA="${WEB_LINKS_BASE_SHA:-}"
DEFAULT_REF="master"

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
    echo "the website's links into this repository were NOT checked" >&2
    exit 2
  fi
  source_label="${SITE_URL}"
fi

# GitHub's heading slug, close enough for headings written in English prose:
# lowercase, drop everything that is not a letter, digit, space, underscore or
# hyphen, then spaces to hyphens. Duplicate headings get a -1 suffix on GitHub;
# the site does not link one.
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
# side asks git and not the filesystem: drivers/*.lua is present in a working
# copy after `make drivers` and 404s on github.com.
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

base=""
removed=""
if [ -n "${BASE_SHA}" ] && git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  base="${BASE_SHA}"
  # Renames count as removals of the old path: the site links the old one.
  removed="$(git diff --name-status --diff-filter=DR "${BASE_SHA}"...HEAD |
    awk '{print $2}' || true)"
fi

removals_under() {
  [ -z "${removed}" ] && return 0
  printf '%s\n' "${removed}" | grep -cE "^$(printf '%s' "$1" | sed -E 's/[][\.^$*+?(){}|/]/\\&/g')(/|$)" || true
}

broken_here=""
broken_already=""
hollowed=""
skipped_refs=""
checked=0

report() { # url reason detail
  local entry
  entry="  $1"$'\n'"      $2"
  [ -n "$3" ] && entry="${entry}"$'\n'"      $3"
  printf '%s\n' "${entry}"
}

# Everything the site points at, deduplicated. The trailing-character strip
# keeps prose punctuation out of a URL that ends a sentence.
links="$(grep -oE 'https://(github\.com|raw\.githubusercontent\.com)/srcfl/ftw[^"'"'"'[:space:]<>)]*' "${site}" |
  sed -E 's/[.,;:]+$//' | sort -u)"

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
      case "${path}" in *#*) anchor="${path#*#}"; path="${path%%#*}" ;; esac
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

  # A link into a branch or tag this repository does not use cannot be checked
  # against the checkout, and is usually a mistake on its own.
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
  # for. This is how the driver move read from here: drivers/ stayed, its 37
  # drivers did not.
  if [ "${kind}" = "tree" ]; then
    gone="$(removals_under "${path}")"
    if [ "${gone:-0}" -gt 0 ]; then
      hollowed="${hollowed}$(report "${url}" \
        "still resolves, but this change removes ${gone} tracked file(s) under ${path}/" "")"$'\n'
    fi
  fi
done <<<"${links}"

echo "website links into this repository: ${checked} checked"
echo "source: ${source_label}"

status=0

if [ -n "${broken_here}" ]; then
  status=1
  echo
  echo "broken by this change:"
  printf '%s' "${broken_here}"
fi

if [ -n "${broken_already}" ]; then
  status=1
  echo
  echo "already broken before this change:"
  printf '%s' "${broken_already}"
fi

if [ -n "${skipped_refs}" ]; then
  status=1
  echo
  echo "pointing at a ref this repository does not publish:"
  printf '%s' "${skipped_refs}"
fi

if [ -n "${hollowed}" ]; then
  echo
  echo "resolving, but no longer holding what the site sends people to see:"
  printf '%s' "${hollowed}"
fi

if [ "${status}" = "0" ] && [ -z "${hollowed}" ]; then
  echo "every one of them resolves against this checkout"
  exit 0
fi

cat <<'MSG'

The website is edited in srcfl/ftw-web, not here, so this check does not fail
the merge -- see the "Website" section of CONTRIBUTING.md. Open a pull request
against that repository in the same sitting:

  https://github.com/srcfl/ftw-web/blob/main/index.html

A heading this README no longer has is not a redirect: GitHub drops the
fragment and leaves the visitor at the top of a long page, which is why a
broken install button can go unnoticed for months.
MSG

exit "${status}"
