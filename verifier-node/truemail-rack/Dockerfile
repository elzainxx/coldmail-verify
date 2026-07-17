# Verifier-node = forked truemail-rack + PINNED truemail gem (Door 5).
# Ruby is pinned to the fork's .ruby-version (3.2). Runs STOCK in Phase 0 —
# no wrapper endpoints (/verify, /audit, /catchall, /config, /stats): those are Phase 2.
#
# The truemail engine is pinned EXACTLY to 3.3.1 in the Gemfile (never forked).
# Gemfile.lock is (re)generated here because the host has no Ruby and the exact
# pin invalidated the upstream lock — the resolved lock is the build artifact.
#
# Base pinned to ruby:3.2.0-slim to MATCH the fork's own `.ruby-version` (ruby-3.2.0):
# the fork's Gemfile reads .ruby-version and pins ruby EXACTLY, so a floating 3.2-slim
# (currently 3.2.11) makes bundler refuse ("Gemfile specified 3.2.0"). Matching the base
# to the fork's declared version honors the fork rather than editing its .ruby-version.
FROM ruby:3.2.0-slim

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

# Resolve gems against the PINNED Gemfile. --jobs 1: under a TLS-interception proxy,
# parallel fetches intermittently fail the compact-index protocol; serial is reliable.
COPY Gemfile .ruby-version ./
RUN gem install bundler -v '~> 2.4' \
    && bundle config set --local without 'development test' \
    && bundle lock \
    && bundle install --jobs 1 --retry 5

# App code (stock fork).
COPY . .

# Stock rack: root '/' validation, '/version', '/healthcheck'. Auth via ACCESS_TOKENS.
# VERIFIER_EMAIL + ACCESS_TOKENS are REQUIRED by the rack; supplied at runtime,
# never baked into the image (secrets discipline — see PROJECT_RULES.md).
EXPOSE 3000
CMD ["sh", "-c", "bundle exec thin -R config.ru -a 0.0.0.0 -p 3000 -e ${RACK_ENV:-production} start"]
