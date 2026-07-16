# Box = forked truemail-rack + PINNED truemail gem (Door 5).
# Ruby is pinned to the fork's .ruby-version (3.2). Runs STOCK in Phase 0 —
# no wrapper endpoints (/verify, /audit, /catchall, /config, /stats): those are Phase 2.
#
# The truemail engine is pinned EXACTLY to 3.3.1 in the Gemfile (never forked).
# Gemfile.lock is (re)generated here because the host has no Ruby and the exact
# pin invalidated the upstream lock — the resolved lock is the build artifact.
FROM ruby:3.2-slim

# thin/eventmachine need a compiler; net-smtp is pure Ruby. Kept minimal.
RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Resolve gems first (layer-cached) against the PINNED Gemfile.
COPY Gemfile .ruby-version ./
RUN gem install bundler -v '~> 2.4' \
    && bundle config set --local without 'development test' \
    && bundle lock \
    && bundle install --jobs 4 --retry 3

# App code (stock fork).
COPY . .

# Stock rack: root '/' validation, '/version', '/healthcheck'. Auth via ACCESS_TOKENS.
# VERIFIER_EMAIL + ACCESS_TOKENS are REQUIRED by the rack; supplied at runtime,
# never baked into the image (secrets discipline — see PROJECT_RULES.md).
EXPOSE 3000
CMD ["sh", "-c", "bundle exec thin -R config.ru -a 0.0.0.0 -p 3000 -e ${RACK_ENV:-production} start"]
