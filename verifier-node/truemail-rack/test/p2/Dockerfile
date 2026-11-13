# Phase-2 test image (test harness, NOT a deployable). Build context = the truemail-rack dir:
#   docker build -f test/p2/Dockerfile -t tmvn-p2-test:local .
# See test/p2/README.md. Extends the built p2 image with
# the test-group gems (rspec) + the socket-level mock substrate (smtp_mock/dns_mock gems + the
# smtpmock Go binary) so we can (a) run the fork's rspec wrapper specs and (b) drive the REAL SMTP
# code path against fakes (Appendix B matrix). The production image never contains any of this.
FROM truemail-verifier-node:p2

USER root
# curl + ca-certificates to fetch the smtpmock release; python3 as a raw-socket probe driver.
RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends curl ca-certificates python3 \
    && rm -rf /var/lib/apt/lists/*

# Trust the same TLS-proxy CA as the base (no-op in a clean env).
COPY certs/ /tmp/proxy-certs/
RUN if ls /tmp/proxy-certs/*.crt >/dev/null 2>&1; then \
      cp /tmp/proxy-certs/*.crt /usr/local/share/ca-certificates/ ; \
      update-ca-certificates ; \
    fi

# Install ALL gems including the test group + the mock substrate gems. We install the mock gems
# ad-hoc (not via the fork Gemfile, which must stay minimal) so the fork's dependency list is
# unchanged — this image is a test harness, not the fork.
RUN bundle config unset --local without \
    && bundle install --jobs 1 --retry 5 \
    && gem install smtp_mock -v '1.4.4' \
    && gem install dns_mock -v '1.7.6'

# Install the smtpmock Go binary via the gem's OWN installer (`smtp_mock -i PATH`) — it knows the
# correct release URL/version and creates the /usr/local/bin/smtpmock symlink the gem's dependency
# check requires (readlink must be non-empty). Done AFTER the gem install so the CLI exists.
# The gem installer drops the real binary at the given path but its dependency check requires
# /usr/local/bin/smtpmock to be a SYMLINK (readlink must be non-empty). So install the real binary
# into /usr/local/lib and symlink it into PATH — satisfying both the check and execution.
RUN rm -f /usr/local/bin/smtpmock /usr/local/lib/smtpmock \
    && smtp_mock -i /usr/local/lib \
    && ln -sf /usr/local/lib/smtpmock /usr/local/bin/smtpmock \
    && test -n "$(readlink /usr/local/bin/smtpmock)" \
    && smtpmock -v
