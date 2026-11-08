#!/usr/bin/env bash
# check-gemfile-lock-selftest — the PAIRED CONTROL for check-gemfile-lock.sh. On a good tree the gate prints `ok`, which means
# every one of its failure branches is unexercised. This builds throwaway fixtures that trip each assertion in turn and checks
# BOTH the exit code and the message (a gate that fails for the wrong reason is not a control).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATE="$HERE/scripts/check-gemfile-lock.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

good_dockerfile() { cat <<'EOF'
FROM ruby:3.2.11-slim-bookworm@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
COPY Gemfile Gemfile.lock .ruby-version ./
RUN gem install bundler -v '2.7.2' \
    && bundle config set --local frozen true \
    && bundle config set --local without 'development test' \
    && bundle install --jobs 1 --retry 5
EOF
}
good_gemfile() { cat <<'EOF'
source 'https://rubygems.org'
gem 'net-smtp', '0.5.1'
gem 'truemail', '3.3.1'
group :development, :test do
  gem 'rspec', '~> 3.12'
end
EOF
}
good_lock() { cat <<'EOF'
GEM
  remote: https://rubygems.org/
  specs:
    net-smtp (0.5.1)
    rspec (3.13.2)
    truemail (3.3.1)

PLATFORMS
  ruby

RUBY VERSION
   ruby 3.2.11p268

BUNDLED WITH
   2.7.2
EOF
}
reset() { good_dockerfile >"$TMP/Dockerfile"; good_gemfile >"$TMP/Gemfile"; good_lock >"$TMP/Gemfile.lock"; printf 'ruby-3.2.11\r\n' >"$TMP/.ruby-version"; }
run() { CHECK_TRACKED=0 bash "$GATE" "$TMP/Dockerfile" "$TMP/Gemfile" "$TMP/Gemfile.lock" "$TMP/.ruby-version" 2>&1; }

pass=0; failn=0
expect_ok()   { local out rc=0; out="$(run)" || rc=$?; if [ $rc -eq 0 ]; then pass=$((pass+1)); echo "  ok   $1"; else failn=$((failn+1)); echo "  BAD  $1 — expected exit 0, got $rc: $out"; fi; }
expect_fail() { local msg="$2" out rc=0; out="$(run)" || rc=$?
  if [ $rc -ne 0 ] && grep -q -- "$msg" <<<"$out"; then pass=$((pass+1)); echo "  ok   $1 (rejected: $msg)"
  else failn=$((failn+1)); echo "  BAD  $1 — expected a failure containing '$msg', got rc=$rc: $out"; fi; }

reset;                                                                          expect_ok   "a correct tree passes (also with the CRLF the fork stores in .ruby-version)"
reset; rm -f "$TMP/Gemfile.lock";                                               expect_fail "a missing lock"                              "missing or empty"
reset; : >"$TMP/Gemfile.lock";                                                  expect_fail "an empty lock"                               "missing or empty"
reset; sed -i 's|^COPY Gemfile Gemfile.lock |COPY Gemfile |' "$TMP/Dockerfile"; expect_fail "a Dockerfile that does not COPY the lock"     "does not COPY Gemfile.lock"
reset; sed -i 's|&& bundle install|\&\& bundle lock \&\& bundle install|' "$TMP/Dockerfile"; expect_fail "the original defect: 'bundle lock' in the image" "re-resolves every gem"
reset; sed -i 's|&& bundle install|\&\& bundle update \&\& bundle install|' "$TMP/Dockerfile"; expect_fail "'bundle update' in the image"           "re-resolves every gem"
reset; sed -i '/frozen true/d' "$TMP/Dockerfile";                               expect_fail "no frozen setting"                           "frozen true"
reset; sed -i 's|truemail (3.3.1)|truemail (3.3.0)|' "$TMP/Gemfile.lock";       expect_fail "lock drifted from the truemail pin (Door 5)" "pins truemail 3.3.1 but the lock has 3.3.0"
reset; sed -i 's|net-smtp (0.5.1)|net-smtp (0.5.0)|' "$TMP/Gemfile.lock";       expect_fail "lock drifted from the net-smtp pin"          "pins net-smtp 0.5.1 but the lock has 0.5.0"
reset; sed -i '/truemail (3.3.1)/d' "$TMP/Gemfile.lock";                        expect_fail "truemail missing from the lock"              "no entry for 'truemail'"
reset; sed -i "s|-v '2.7.2'|-v '~> 2.4'|" "$TMP/Dockerfile";                    expect_fail "bundler installed by a range"                "by a RANGE"
reset; sed -i "s|-v '2.7.2'|-v '2.6.9'|" "$TMP/Dockerfile";                     expect_fail "bundler version != BUNDLED WITH"             "BUNDLED WITH 2.7.2"
reset; printf 'ruby-3.3.1\n' >"$TMP/.ruby-version";                              expect_fail "lock written for a different Ruby"           "written for Ruby 3.2.11 but .ruby-version names 3.3.1"
reset; sed -i '/rspec (3.13.2)/d' "$TMP/Gemfile.lock";                          expect_fail "no dev/test gems in the lock"                "no development/test gems"

echo "check-gemfile-lock-selftest: $pass passed, $failn failed"
[ "$failn" -eq 0 ]
