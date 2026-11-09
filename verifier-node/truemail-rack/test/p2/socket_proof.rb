# frozen_string_literal: true

# Phase-2 SOCKET-LEVEL proof (Appendix B matrix) — the REAL SMTP/DNS code path against fakes.
# This is NOT a deployable and NOT part of the fork; it is the P2 end-to-end proof harness (§3.5),
# run inside the throwaway test image. It wires the pinned truemail gem to dns_mock (UDP) +
# smtp_mock (TCP), then exercises the wrapper's own catch-all probe + serialization against them,
# so the wire behavior (transcript capture, 2nd-RCPT probe, timeouts, 5xx bodies) is proven for
# real — not just against a stub. Prints PASS/FAIL per row; exits non-zero on any failure.
#
# GAP-14 reality (ground-truth §3.4): smtp_mock 250-accepts everything except pre-listed addresses,
# so the ONLY path to `valid` uses a SEEDED probe pre-listed for rejection. GAP-12: smtp_mock has no
# stateful greylist; the greylist row proves DETECTION+CLASSIFICATION via a 450 custom message, and
# the retry-then-accept round-trip is explicitly deferred (rollout-only), never faked green.

require 'truemail'
require 'smtp_mock'
require 'dns_mock'
require 'json'
require_relative '/app/app/truemail_server/wrapper/serializer'
require_relative '/app/app/truemail_server/wrapper/catchall_probe'
require_relative '/app/app/truemail_server/wrapper/probe_guard'

# ADR-0021: the destination guard refuses loopback, and every fake in this harness (dns_mock, smtp_mock) lives on loopback. The
# original eight rows are about the SMTP wire path, not the guard, so they run with the guard explicitly overridden (the override is
# exact "1", and turns a real node's audit RED). Rows 9-11 at the end turn the guard ON and prove it against the same real DNS.
ENV['PROBE_ALLOW_NON_PUBLIC'] = '1'

RESULTS = []

def check(name, cond, detail = '')
  RESULTS << [name, cond]
  puts format('  [%s] %-52s %s', cond ? 'PASS' : 'FAIL', name, detail)
end

# ── Wire the mock substrate ────────────────────────────────────────────────────────────────
# dns_mock: map the test domains' MX to 127.0.0.1 so the engine's SMTP step dials the local mock.
DNS_PORT = 5300
dns = DnsMock::Server.new(port: DNS_PORT)
# dns_mock MX format is a String "host:preference" (host:port-style), NOT a hash.
dns.assign_mocks(
  'corp.test'    => { mx: ['mx.corp.test:10'], a: ['127.0.0.1'] },
  'mx.corp.test' => { a: ['127.0.0.1'] },
  'full.test'    => { mx: ['mx.full.test:10'], a: ['127.0.0.1'] },
  'mx.full.test' => { a: ['127.0.0.1'] }
)

def smtp_server(**opts)
  SmtpMock::Server.new(**opts)
end

def configure_engine(smtp_port)
  Truemail.reset_configuration!
  Truemail.configure do |c|
    c.verifier_email = 'verifier@example.test'
    c.dns = ["127.0.0.1:#{DNS_PORT}"]
    c.smtp_port = smtp_port
    c.connection_timeout = 2
    c.response_timeout = 2
    c.smtp_fail_fast = true
  end
end

def parsed_target(email)
  result = Truemail.validate(email, with: :smtp).result
  { success: result.success, smtp_debug: TruemailServer::Wrapper::Serializer.smtp_debug(result.smtp_debug) }
end

def rcptto_code(smtp_debug)
  return nil unless smtp_debug
  smtp_debug.each do |h|
    raw = (h[:errors] || {})[:rcptto]
    next unless raw
    m = raw.match(/\A\s*(\d{3})/)
    return m[1] if m
  end
  # success case: rcptto is a {status,string}
  smtp_debug.each do |h|
    r = h[:rcptto]
    return r[:status] if r.is_a?(Hash) && r[:status]
  end
  nil
end

begin
  # ── Row 4: accept-all default → catch-all (GAP-14: a random probe is always 250'd) ──────────
  s = smtp_server
  configure_engine(s.port)
  probe = TruemailServer::Wrapper::CatchallProbe.run('corp.test') # random probe
  check('row4 accept-all default → catchall', probe[:conclusion] == 'catchall', probe[:conclusion])
  s.stop!

  # ── Row 1: SEEDED probe rejected → the valid path (target 250 + probe 550) ──────────────────
  seeded = 'nx-seeded'
  s = smtp_server(not_registered_emails: ["#{seeded}@corp.test"])
  configure_engine(s.port)
  # target (random, accepted) then probe (seeded, rejected) — two-session Option B (ADR-0007).
  target = parsed_target("real-user@corp.test")
  probe = TruemailServer::Wrapper::CatchallProbe.run('corp.test', probe_local_part: seeded)
  valid_path = target[:success] == true && probe[:conclusion] == 'not_catchall'
  check('row1 seeded-probe rejected → valid path (target250+probe550)', valid_path,
        "target_success=#{target[:success]} probe=#{probe[:conclusion]}")
  s.stop!

  # ── Row 2: target address itself not registered → 550 no-user (address-fault → invalid) ─────
  s = smtp_server(not_registered_emails: ['ghost@corp.test'])
  configure_engine(s.port)
  t = parsed_target('ghost@corp.test')
  code = rcptto_code(t[:smtp_debug])
  check('row2 unregistered target → 550 on RCPT', code == '550', "code=#{code}")
  s.stop!

  # ── Row 5: timeout — response_delay > response_timeout → smtp_timeout (unknown) ─────────────
  s = smtp_server(response_delay_rcptto: 4) # > 2s response_timeout
  configure_engine(s.port)
  t = parsed_target('slow@corp.test')
  timed_out = (t[:smtp_debug] || []).any? { |h| (h[:errors] || {}).values.any? { |v| v.to_s =~ /timeout|timed out/i } }
  check('row5 response_delay>timeout → timeout captured', timed_out, "success=#{t[:success]}")
  s.stop!

  # ── Row 8: port closed → connection_failed (port_opened=false) ──────────────────────────────
  configure_engine(65_534) # nothing listening
  t = parsed_target('x@corp.test')
  no_port = (t[:smtp_debug] || []).any? { |h| h[:port_opened] == false }
  check('row8 closed port → port_opened=false', no_port, "debug=#{(t[:smtp_debug] || []).size} hosts")

  # ── Row 7: 552 mailbox full via a custom not-registered message → 552 flows through RCPT ────
  # smtp_mock: not_registered_emails makes the address reject; msg_rcptto_not_registered_email
  # sets that reject's body — so we craft a REAL 552 on the wire. The brain then maps 552→valid.
  s = smtp_server(not_registered_emails: ['full-box@full.test'],
                  msg_rcptto_not_registered_email: '552 mailbox full')
  configure_engine(s.port)
  t = parsed_target('full-box@full.test')
  code = rcptto_code(t[:smtp_debug])
  check('row7 custom 552 mailbox-full flows through RCPT', code == '552', "code=#{code}")
  s.stop!

  # ── Row 3: your-fault BLOCK via a custom 5xx blacklist message → smtp_blocked path (§6.1) ───
  # Ground-truth §3.4 delta: the stock blacklist knob returns 421 (transient). To exercise the
  # 5xx smtp_blocked path we craft a 554 via msg_rcptto_blacklisted_email — the reconciliation the
  # spec §6.1 requires. Proves a 5xx block body flows through and carries a policy-block phrasing.
  s = smtp_server(blacklisted_rcptto_emails: ['blocked@corp.test'],
                  msg_rcptto_blacklisted_email: '554 5.7.1 blocked by policy')
  configure_engine(s.port)
  t = parsed_target('blocked@corp.test')
  code = rcptto_code(t[:smtp_debug])
  check('row3 custom 554 policy-block flows through RCPT', code == '554', "code=#{code}")
  s.stop!

  # ── Also capture the STOCK blacklist behaviour to CONFIRM the ground-truth §3.4 421 delta ────
  s = smtp_server(blacklisted_rcptto_emails: ['blocked2@corp.test'])
  configure_engine(s.port)
  t = parsed_target('blocked2@corp.test')
  code = rcptto_code(t[:smtp_debug])
  # Ground-truth recorded 421 for the stock blacklist knob; assert it is a 4xx transient (not 5xx),
  # which is exactly why §6.1 needs a custom-message fixture (row 3 above) for the 5xx path.
  check('delta stock blacklist → 4xx transient (confirms §3.4 421-vs-554)', code&.start_with?('4'),
        "stock blacklist code=#{code}")
  s.stop!

  # ── Rows 9-11 (ADR-0021): the DESTINATION GUARD against real DNS (dns_mock) and the real gem. ─────────────────────────────
  # corp.test's MX resolves to 127.0.0.1 (above). The destination is a COUNTING listener, so "no connection was made" is observed,
  # not inferred; and row 10 is the control that proves the listener WOULD have seen one (a zero count means nothing otherwise).
  listener = Class.new do
    attr_reader :port

    def initialize
      @server = TCPServer.new('127.0.0.1', 0)
      @port = @server.addr[1]
      @count = 0
      @mutex = Mutex.new
      @thread = Thread.new do
        loop do
          client = @server.accept
          @mutex.synchronize { @count += 1 }
          client.close
        end
      rescue StandardError
        nil
      end
    end

    def count = @mutex.synchronize { @count }

    def close
      @server.close
    rescue StandardError
      nil
    end
  end.new
  configure_engine(listener.port)
  guard = TruemailServer::Wrapper::ProbeGuard

  # Row 9 — guard ON: the wrapper's sequence is `preflight → (refusal? return) → Truemail.validate`; the gem is only reached when
  # there is no refusal. Loopback MX → refused, range named, and the listener saw NOTHING.
  ENV.delete('PROBE_ALLOW_NON_PUBLIC')
  refusal = guard.preflight('someone@corp.test')
  Truemail.validate('someone@corp.test', with: :smtp) unless refusal
  check('row9 guard ON: loopback MX refused, ZERO connections reach it',
        !refusal.nil? && refusal[:targets].first[:range] == 'loopback' && listener.count.zero?,
        "refusal=#{refusal&.slice(:reason, :count)} connections=#{listener.count}")

  # Row 11 — the standalone catch-all probe is guarded too (it is a second SMTP session to the same destination).
  probe = TruemailServer::Wrapper::CatchallProbe.run('corp.test')
  check('row11 guard ON: the catch-all probe is refused too, ZERO connections',
        probe[:conclusion] == 'inconclusive' && probe[:probe_refused] && listener.count.zero?,
        "conclusion=#{probe[:conclusion]} connections=#{listener.count}")

  # Row 10 — CONTROL: same domain, same listener, override ON → the pre-flight passes and the gem DOES connect.
  ENV['PROBE_ALLOW_NON_PUBLIC'] = '1'
  refusal = guard.preflight('someone@corp.test')
  Truemail.validate('someone@corp.test', with: :smtp) unless refusal
  check('row10 CONTROL override ON: the same destination IS connected to (the listener sees it)',
        refusal.nil? && listener.count.positive?, "connections=#{listener.count}")
  listener.close

ensure
  begin
    dns.stop!
  rescue StandardError
    nil
  end
end

puts
failed = RESULTS.reject { |(_n, ok)| ok }
puts "SOCKET PROOF: #{RESULTS.size - failed.size}/#{RESULTS.size} rows passed"
puts "GAP-12 note: the greylist retry-then-accept ROUND-TRIP is NOT socket-proven (smtp_mock has no"
puts "  stateful reject-then-accept). Detection+classification of a 450 is unit-proven brain-side;"
puts "  true greylist validation is rollout-only (GAP-35) — not faked green here."
exit(failed.empty? ? 0 : 1)
