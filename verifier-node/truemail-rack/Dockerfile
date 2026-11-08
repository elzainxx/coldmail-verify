# Verifier-node = forked truemail-rack + PINNED truemail gem (Door 5).
# Ruby is pinned to the fork's .ruby-version (3.2). Runs STOCK in Phase 0 —
# no wrapper endpoints (/verify, /audit, /catchall, /config, /stats): those are Phase 2.
#
# The truemail engine is pinned EXACTLY to 3.3.1 in the Gemfile (never forked).
# Gemfile.lock is (re)generated here because the host has no Ruby and the exact
# pin invalidated the upstream lock — the resolved lock is the build artifact.
#
# Base: the latest Ruby 3.2 patch on Debian 12 (bookworm), PINNED BY DIGEST (ADR-0018).
#
# WHY NOT ruby:3.2.0-slim any more. That image is Debian 11 (bullseye); Debian removed bullseye's `debian-security`
# repository, so `apt-get install` returns 404 on any build that does not hit a cached layer. It stayed invisible on a
# developer machine (Docker reused cached layers) and was found the first time GitHub Actions built the image from
# scratch. No 3.2.0 image exists on bookworm, so the base moves to the last 3.2 patch there (3.2.11) and the fork's
# `.ruby-version` — which the Gemfile reads and pins Ruby to EXACTLY — moves with it (FORK.md row 14). Same minor
# version. Door 5 is unaffected: it protects the truemail GEM (pinned 3.3.1, asserted unmodified), not the OS image.
#
# WHY A DIGEST. A tag is a moving target; a digest is a content address. `ruby:3.2.11-slim-bookworm` and the digest
# below are the same image TODAY; only the digest stays the same image tomorrow. CI builds with `--pull --no-cache`
# (and scripts/check-base-image-pinned.sh enforces both), so a dead or changed base can never hide behind a cache again.
# To move to a newer base: pull it, take its digest, change this line and `.ruby-version` together, re-run CI.
FROM ruby:3.2.11-slim-bookworm@sha256:84140472007d27c7ec641dbc606a47ff9c0aa5f358445947b7350392fa5115e5

# thin/eventmachine need a compiler; net-smtp is pure Ruby. ca-certificates lets us
# trust an optional corporate/AV TLS-interception proxy CA (see below). Kept minimal.
RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# OPTIONAL TLS-proxy trust (portable — no-op in clean environments like CI).
# Some hosts run an SSL/TLS-scanning AV/proxy (e.g. Norton) that re-signs HTTPS with a
# private root CA; without trusting it, in-container gem/bundle fetches fail cert
# verification against rubygems.org. Any *.crt placed in ./certs/ is trusted — so we
# TRUST the same proxy the host does; we do NOT disable TLS verification.
# The dir always exists (certs/.keep); machine-specific certs are gitignored, so a clean
# checkout/CI copies only .keep and the trust step below is a harmless no-op.
# Install any provided proxy CA(s) the DEBIAN-CORRECT way: drop *.crt into
# /usr/local/share/ca-certificates/ (flat, NOT a subdir) and run update-ca-certificates,
# which regenerates the hashed /etc/ssl/certs dir OpenSSL/Ruby actually read. (Appending to
# ca-certificates.crt, or using a subdir, does NOT take effect on this base — verified.)
COPY certs/ /tmp/proxy-certs/
RUN if ls /tmp/proxy-certs/*.crt >/dev/null 2>&1; then \
      cp /tmp/proxy-certs/*.crt /usr/local/share/ca-certificates/ ; \
      update-ca-certificates ; \
    fi

WORKDIR /app

# Install EXACTLY the gems in the committed Gemfile.lock (ADR-0018 addendum). This used to run `bundle lock` here, which
# re-resolved every transitive gem on every build: only truemail (3.3.1) and net-smtp were pinned, so everything else —
# rack, thin, eventmachine, dry-* — floated to whatever resolved that day, and a digest-pinned base made the OS layer
# reproducible while the gems above it were not. Now the lock is the source of truth and `frozen` makes bundler FAIL
# if the Gemfile and the lock ever disagree, instead of quietly re-resolving. bundler itself is pinned to the version the
# lock was written by (its BUNDLED WITH line; scripts/check-gemfile-lock.sh asserts they match). The lock covers the
# development/test groups too, so the RSpec job's `bundle config unset --local without && bundle install` still works.
# To change a gem: edit the Gemfile, run `bundle lock` in this image's Ruby, commit both, re-run CI.
# --jobs 1: under a TLS-interception proxy, parallel fetches intermittently fail the compact-index protocol; serial is reliable.
COPY Gemfile Gemfile.lock .ruby-version ./
RUN gem install bundler -v '2.7.2' \
    && bundle config set --local frozen true \
    && bundle config set --local without 'development test' \
    && bundle install --jobs 1 --retry 5

# App code (stock fork).
COPY . .

# Stock rack: root '/' validation, '/version', '/healthcheck'. Auth via ACCESS_TOKENS.
# VERIFIER_EMAIL + ACCESS_TOKENS are REQUIRED by the rack; supplied at runtime,
# never baked into the image (secrets discipline — see PROJECT_RULES.md).
EXPOSE 3000
CMD ["sh", "-c", "bundle exec thin -R config.ru -a 0.0.0.0 -p 3000 -e ${RACK_ENV:-production} start"]
