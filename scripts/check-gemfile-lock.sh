#!/usr/bin/env bash
# check-gemfile-lock — BLOCKING CI gate (ADR-0018 addendum): the verifier-node image installs EXACTLY the gems in a committed
# Gemfile.lock, and the lock agrees with the Gemfile's exact pins.
#
# THE DEFECT. The Dockerfile ran `bundle lock` on every build, so only `truemail` (3.3.1) and `net-smtp` (0.5.1) were pinned;
# every other gem — rack, thin, eventmachine, the dry-* family — floated to whatever resolved that day. A digest-pinned base
# image (ADR-0018) made the OS layer reproducible while the gems above it were not: the same hidden-variable shape, one layer up.
# A cold build on a CI runner and a warm one on a developer's machine could install different code under the same Gemfile.
#
# WHAT IT ASSERTS, and why each:
#   1. Gemfile.lock exists, is non-empty, and git TRACKS it (an ignored or uncommitted lock is no lock).
#   2. the Dockerfile COPYs Gemfile.lock and runs no `bundle lock` / `bundle update` — those would re-resolve and discard the lock.
#   3. the Dockerfile sets `bundle config set --local frozen true`, so bundler FAILS if the Gemfile and lock diverge instead of
#      quietly re-resolving.
#   4. the lock pins `truemail` and `net-smtp` to the same versions the Gemfile pins EXACTLY — Door 5's pin cannot drift from
#      what is actually installed.
#   5. the Dockerfile installs bundler at an EXACT version equal to the lock's `BUNDLED WITH` (a `~>` range would let the
#      bundler that reads the lock change under it).
#   6. the lock's `RUBY VERSION` is the patch .ruby-version names.
#   7. the lock includes the development/test gems (rspec): the RSpec job runs `bundle config unset without && bundle install`
#      and would otherwise re-resolve at test time.
# Paired control: scripts/check-gemfile-lock-selftest.sh proves each assertion can FAIL.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP="$HERE/verifier-node/truemail-rack"
DOCKERFILE="${1:-$APP/Dockerfile}"
GEMFILE="${2:-$APP/Gemfile}"
LOCK="${3:-$APP/Gemfile.lock}"
RUBYVER="${4:-$APP/.ruby-version}"
CHECK_TRACKED="${CHECK_TRACKED:-1}"

fail=0
bad() { echo "FAIL (gemfile-lock): $*" >&2; fail=1; }

# 1
[ -s "$LOCK" ] || bad "Gemfile.lock is missing or empty: $LOCK"
if [ "$CHECK_TRACKED" = 1 ] && [ -s "$LOCK" ]; then
  git -C "$HERE" ls-files --error-unmatch -- "${LOCK#"$HERE"/}" >/dev/null 2>&1 \
    || bad "Gemfile.lock exists but git does not track it (ignored or uncommitted): ${LOCK#"$HERE"/}"
fi

# 2
grep -Eq '^COPY[[:space:]]+Gemfile[[:space:]]+Gemfile\.lock([[:space:]]|$)' "$DOCKERFILE" \
  || bad "the Dockerfile does not COPY Gemfile.lock next to the Gemfile (expected 'COPY Gemfile Gemfile.lock …')"
if grep -E '^[^#]*bundle[[:space:]]+(lock|update)\b' "$DOCKERFILE" | grep -vq '^[[:space:]]*#'; then
  bad "the Dockerfile runs 'bundle lock' or 'bundle update', which re-resolves every gem and discards the committed lock: $(grep -E '^[^#]*bundle[[:space:]]+(lock|update)\b' "$DOCKERFILE" | head -1 | sed 's/^[[:space:]]*//')"
fi

# 3
grep -Eq 'bundle[[:space:]]+config[[:space:]]+set[[:space:]]+--local[[:space:]]+frozen[[:space:]]+true' "$DOCKERFILE" \
  || bad "the Dockerfile does not set 'bundle config set --local frozen true' (bundler would silently re-resolve if the lock drifts)"

# 4
pin() { grep -E "^[[:space:]]*gem[[:space:]]+['\"]$1['\"]" "$GEMFILE" | head -1 | sed -E "s/.*['\"]$1['\"][[:space:]]*,[[:space:]]*['\"]([0-9][^'\"]*)['\"].*/\1/" | tr -d '\r'; }
locked() { sed -n -E "s/^    $1 \(([0-9][^)]*)\)\$/\1/p" "$LOCK" 2>/dev/null | head -1 | tr -d '\r'; }
for g in truemail net-smtp; do
  want="$(pin "$g")"; have="$(locked "$g")"
  [ -n "$want" ] || { bad "the Gemfile does not pin '$g' to an exact version"; continue; }
  [ -n "$have" ] || { bad "the lock has no entry for '$g'"; continue; }
  [ "$want" = "$have" ] || bad "the Gemfile pins $g $want but the lock has $have"
done

# 5
bundled="$(sed -n '/^BUNDLED WITH/{n;s/^[[:space:]]*//;p;}' "$LOCK" 2>/dev/null | head -1 | tr -d '\r')"
bv="$(grep -E 'gem[[:space:]]+install[[:space:]]+bundler' "$DOCKERFILE" | head -1 | sed -nE "s/.*-v[[:space:]]+['\"]?([0-9][0-9.]*)['\"]?.*/\1/p")"
if grep -E 'gem[[:space:]]+install[[:space:]]+bundler' "$DOCKERFILE" | grep -Eq -- "-v[[:space:]]+['\"]?[~><=]"; then
  bad "the Dockerfile installs bundler by a RANGE ($(grep -E 'gem[[:space:]]+install[[:space:]]+bundler' "$DOCKERFILE" | head -1 | sed 's/^[[:space:]]*//')); it must be the exact version the lock was written by"
elif [ -z "$bv" ]; then
  bad "the Dockerfile does not install a specific bundler version"
elif [ -z "$bundled" ]; then
  bad "the lock has no BUNDLED WITH line"
elif [ "$bv" != "$bundled" ]; then
  bad "the Dockerfile installs bundler $bv but the lock says BUNDLED WITH $bundled"
fi

# 6
want_ruby="$(tr -d '\r\n' <"$RUBYVER" | sed -n 's/^ruby-//p')"
lock_ruby="$(sed -n '/^RUBY VERSION/{n;s/^[[:space:]]*ruby[[:space:]]*//;p;}' "$LOCK" 2>/dev/null | head -1 | sed -E 's/p[0-9]+$//' | tr -d '\r')"
[ -n "$want_ruby" ] || bad "could not read a version from $RUBYVER"
[ -n "$lock_ruby" ] || bad "the lock has no RUBY VERSION section"
{ [ -z "$want_ruby" ] || [ -z "$lock_ruby" ] || [ "$want_ruby" = "$lock_ruby" ]; } || bad "the lock was written for Ruby $lock_ruby but .ruby-version names $want_ruby"

# 7
grep -Eq '^    rspec \(' "$LOCK" 2>/dev/null || bad "the lock has no development/test gems (rspec): the RSpec job would re-resolve at test time"

[ "$fail" -eq 0 ] || exit 1
echo "gemfile-lock: ok — lock tracked; image installs it frozen (no 'bundle lock'); truemail $(locked truemail) and net-smtp $(locked net-smtp) match the Gemfile pins; bundler $bundled; Ruby $lock_ruby; dev/test gems present."
