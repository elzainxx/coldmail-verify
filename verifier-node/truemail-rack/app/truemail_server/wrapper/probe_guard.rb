# frozen_string_literal: true

require 'ipaddr'

# FORK EDIT (ADR-0021): the SMTP probe's DESTINATION guard. Pre-flight in the wrapper, BEFORE the gem opens a connection.
#
# THE HOLE. The node connects to whatever address a user-supplied domain's mail host resolves to. A user can publish
# `MX 10 evil.example.` with `evil.example. A 169.254.169.254` (or 127.0.0.1, 10.x.x.x, 100.64.x.x ...) and the verifier
# box opens an SMTP session to it — and the SMTP transcript is persisted verbatim in the raw sidecar, so an internal SMTP
# banner would be recorded. Port 25 only, but that is exactly the port internal mail relays listen on. The gem has no guard
# (searched: `IPAddr` appears in it only in an exception rescue and the PTR audit), and Door 5 forbids touching the gem.
#
# THE SHAPE OF THE CONTROL, in three layers (defence in depth; the firewall in the runbook is the fourth and the only one that
# cannot be raced):
#   1. PRE-FLIGHT  — resolve the target with the GEM'S OWN MX step (`Truemail.validate(email, with: :mx)`), so the candidate
#                    addresses are exactly the ones the SMTP step would try (MX -> CNAME -> implicit A, A and AAAA). If ANY is
#                    non-public the probe is refused and the gem's SMTP step never runs. ANY, not "only": the gem tries every
#                    address in order, so one public MX beside a private one would still reach the private one.
#   2. POST-CHECK  — `Smtp#run` re-runs the MX step (MxBlacklist -> Mx) and APPENDS any newly resolved address to
#                    result.mail_servers, so a DNS answer that flips between the pre-flight and the connect (rebinding) can add a
#                    private address the pre-flight never saw. After the run, the addresses the gem actually used are checked
#                    again; if any is non-public the transcript is DISCARDED (never returned, so never persisted) and the probe
#                    is reported as refused. This does not prevent the connection in that race — it prevents the data leak and
#                    makes the event visible. Only an egress policy prevents the connection.
#   3. AUDIT       — the override below turns the host audit red.
#
# THE OVERRIDE. The socket-level proof harness (test/p2) serves its smtp_mock/dns_mock on loopback, which this guard rightly
# refuses. PROBE_ALLOW_NON_PUBLIC=1 switches the guard off for that harness. It is exact ("1" and nothing else), read at call
# time, and — the point — it turns `GET /v1/audit` RED (warnings.probe_guard), so a node running with the guard off is never
# :smtp-eligible and receives no brain traffic. A deployment cannot quietly run unguarded.
#
# The wrapper renders no verdict. It reports a FACT (`probe_refused`); the brain turns it into `risky` with the reason
# `non_public_mx` (ADR-0021).
module TruemailServer
  module Wrapper
    module ProbeGuard
      ALLOW_ENV = 'PROBE_ALLOW_NON_PUBLIC'
      REASON = 'non_public_target'
      WARNING_KEY = :probe_guard
      MAX_REPORTED = 8

      # IPv4: every IANA special-purpose block that is not globally routable (RFC 6890 and updates), plus multicast and
      # the reserved/broadcast space. Cloud metadata endpoints fall inside these: 169.254.169.254 (AWS/GCP/Azure/OCI-link),
      # 100.100.100.200 (Alibaba, CGNAT), 192.0.0.192 (Oracle, protocol assignments).
      V4_RANGES = {
        '0.0.0.0/8' => 'this-network',
        '10.0.0.0/8' => 'private-rfc1918',
        '100.64.0.0/10' => 'cgnat',
        '127.0.0.0/8' => 'loopback',
        '169.254.0.0/16' => 'link-local/cloud-metadata',
        '172.16.0.0/12' => 'private-rfc1918',
        '192.0.0.0/24' => 'ietf-protocol-assignments',
        '192.0.2.0/24' => 'documentation-test-net-1',
        '192.88.99.0/24' => '6to4-relay-anycast',
        '192.168.0.0/16' => 'private-rfc1918',
        '198.18.0.0/15' => 'benchmarking',
        '198.51.100.0/24' => 'documentation-test-net-2',
        '203.0.113.0/24' => 'documentation-test-net-3',
        '224.0.0.0/4' => 'multicast',
        '240.0.0.0/4' => 'reserved/broadcast'
      }.freeze

      # IPv6 is an ALLOW-list: only global unicast (2000::/3) is public, minus the special-purpose blocks inside it. Everything
      # outside 2000::/3 (loopback ::1, unspecified ::, ULA fc00::/7, link-local fe80::/10, site-local fec0::/10, multicast
      # ff00::/8, ...) is non-public by construction. Deny-listing IPv6 invites gaps; this cannot have one outside 2000::/3.
      V6_GLOBAL = ::IPAddr.new('2000::/3')
      V6_SPECIAL_INSIDE_GLOBAL = {
        '2001::/23' => 'ietf-protocol-assignments/teredo',
        '2001:db8::/32' => 'documentation',
        '2002::/16' => '6to4',
        '3fff::/20' => 'documentation'
      }.freeze
      # Outside 2000::/3 but named so a refusal says WHY rather than "not global unicast":
      V6_NAMED_OUTSIDE = {
        '::/128' => 'unspecified', '::1/128' => 'loopback', '::/96' => 'ipv4-compatible', '64:ff9b::/96' => 'nat64',
        '64:ff9b:1::/48' => 'nat64-local', '100::/64' => 'discard-only', '5f00::/16' => 'srv6-sids',
        'fc00::/7' => 'ula-private', 'fe80::/10' => 'link-local', 'fec0::/10' => 'site-local', 'ff00::/8' => 'multicast'
      }.freeze

      V4 = V4_RANGES.map { |cidr, label| [::IPAddr.new(cidr), label] }.freeze
      V6_INSIDE = V6_SPECIAL_INSIDE_GLOBAL.map { |cidr, label| [::IPAddr.new(cidr), label] }.freeze
      V6_OUTSIDE = V6_NAMED_OUTSIDE.map { |cidr, label| [::IPAddr.new(cidr), label] }.freeze

      module_function

      def override?
        ::ENV[ALLOW_ENV] == '1'
      end

      # nil when `address` is a public address; otherwise a label naming the non-public range. Anything that does not parse as an
      # IP address (a scope id, a hostname, garbage) is non-public: the guard refuses what it cannot prove public.
      def non_public_range(address)
        ip = ::IPAddr.new(address.to_s.strip)
        return classify_v4(ip) if ip.ipv4?
        return classify_v4(ip.native) if ip.ipv4_mapped? # ::ffff:a.b.c.d — judged as the IPv4 address it will connect to

        classify_v6(ip)
      rescue ::IPAddr::Error, ::ArgumentError
        'unparseable-address'
      end

      def classify_v4(ip)
        hit = V4.find { |range, _| range.include?(ip) }
        hit && hit[1]
      end

      def classify_v6(ip)
        named = V6_OUTSIDE.find { |range, _| range.include?(ip) }
        return named[1] if named
        return 'not-global-unicast' unless V6_GLOBAL.include?(ip)

        inside = V6_INSIDE.find { |range, _| range.include?(ip) }
        inside && inside[1]
      end

      # nil when every address is public (or the guard is overridden); else the refusal FACT the wrapper reports:
      #   { reason:, count:, targets: [{ address:, range: }, ...] }   (capped; the full set is not needed to explain a refusal)
      def refusal(addresses)
        return nil if override?

        bad = Array(addresses).filter_map do |a|
          range = non_public_range(a)
          { address: a.to_s, range: range } if range
        end
        return nil if bad.empty?

        { reason: REASON, count: bad.size, targets: bad.first(MAX_REPORTED) }
      end

      # PRE-FLIGHT. Resolve with the gem's own MX step and judge what it found. An exception here propagates (the route's rescue
      # turns it into a 500, which the brain maps to unknown/connection_failed): "could not determine the target" is not "public".
      def preflight(email)
        return nil if override?

        refusal(::Truemail.validate(email, with: :mx).result.mail_servers)
      end

      # POST-CHECK. The addresses the SMTP run actually accumulated (see layer 2 above).
      def postcheck(result)
        refusal(result.mail_servers)
      end

      # The host-audit warning while the guard is off. nil when it is on.
      def override_warning
        return nil unless override?

        "#{ALLOW_ENV}=1: the SMTP probe's destination guard is OFF (ADR-0021). Only the socket-level proof harness may run " \
          'this way; a node with the guard off must receive no brain traffic.'
      end
    end
  end
end
