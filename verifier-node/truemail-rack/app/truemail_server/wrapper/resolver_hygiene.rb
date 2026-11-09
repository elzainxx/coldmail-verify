# frozen_string_literal: true

require 'socket'

# FORK EDIT (ADR-0019 addendum): the node's resolver hygiene, as a CONTROL on the host-audit gate.
#
# WHY. truemail resolves with Ruby's Resolv (Truemail::Dns::Worker < Resolv::DNS), which the gem hands unrooted names. Given a
# search domain, Resolv falls through to a suffixed name on NXDOMAIN, and a wildcard MX under that domain answers for a domain the
# user never typed (measured: a nonexistent name resolved to ANOTHER domain's MX). The gem cannot be changed (Door 5), and until
# now the only defence was a sentence in a runbook: "run the node with no search domain and without a dotted container hostname".
# A documented deployment rule is not a control. This makes it one, by putting it where the module already refuses to route SMTP:
# the audit. A non-empty warnings map is `audit_status: red` (the brain re-derives that itself), the first red quarantines the node, and a
# node that is not active+green is never :smtp-eligible (smtp_skipped_reason=no_eligible_node, or audit_red while still active): a node
# whose resolver is not clean does no SMTP.
#
# WHAT IT LOOKS AT, and why each is here (Ruby 3.2's Resolv::DNS::Config, read in the image, is the source of truth):
#   * a `search` line in resolv.conf    -> an explicit search list.
#   * a `domain` line in resolv.conf    -> a one-entry search list (Resolv treats `domain` as `search`).
#   * `options ndots:N` with N > 1      -> Resolv tries SUFFIXED candidates BEFORE the name as written for any name with fewer than
#                                          N dots (measured: with ndots:2 and a search domain, `example.com` is asked as
#                                          `example.com.<search>` first). Harmless alone, hazardous with any search domain.
#   * a hostname that contains a dot    -> NOT in the brief's list, ADDED, and the reason matters: whenever no explicit :search is
#                                          in effect, Resolv derives one from the HOSTNAME's domain part (`verifier.example.com` ->
#                                          `example.com`). That is every box whose resolv.conf is clean, and EVERY box where truemail
#                                          is given `dns:` servers (it then hands Resolv a Hash and never reads resolv.conf). A
#                                          PTR-compliant mail box very often has an FQDN hostname, so checking resolv.conf alone
#                                          would pass exactly the boxes most likely to be affected. ADR-0019 already named it in the
#                                          rule ("without a dotted container hostname"); the control now covers the whole rule.
#
# DELIBERATELY OVER-APPROXIMATE. A `search` line is flagged even when truemail is configured with `dns:` servers (and so never
# reads resolv.conf): the gate refuses on "not provably clean", never on "provably harmful". scripts/check-resolver-hygiene.sh
# uses Ruby's own resolver as an ORACLE to prove the converse that matters — this rule never MISSES a search domain Ruby would use.
#
# The wrapper still renders no verdict and patches nothing: this only adds a fact to the audit's warnings map (Door 5 intact).
module TruemailServer
  module Wrapper
    module ResolverHygiene
      RESOLV_CONF = '/etc/resolv.conf'
      WARNING_KEY = :resolver

      # Returns nil when the resolver is clean, else ONE human-readable string naming every problem found (it becomes the value of
      # warnings[:resolver]). `resolv_conf` is a path; `hostname` is injectable so specs never depend on the machine they run on.
      def self.check(resolv_conf: RESOLV_CONF, hostname: nil)
        problems = file_problems(resolv_conf)
        host = hostname.nil? ? ::Socket.gethostname : hostname
        problems << "hostname #{host.inspect} contains a dot (Ruby derives a search domain from it)" if host.to_s.include?('.')
        return nil if problems.empty?

        "node resolver is not clean: #{problems.join('; ')}. The node must run with no search domain, no `domain` line, " \
          'ndots <= 1 and a hostname with no dot (ADR-0019), or it does no SMTP.'
      end

      # The resolv.conf half. A missing file is a determinate fact (no configuration -> nothing to flag from it); a file that exists
      # but cannot be read is NOT, and the gate does not guess: unreadable is a problem.
      def self.file_problems(path)
        analyze(::File.read(path))
      rescue ::Errno::ENOENT
        []
      rescue ::SystemCallError, ::IOError => e
        ["cannot read #{path} (#{e.class.name.split('::').last}), so it cannot be shown to be clean"]
      end

      # Pure: resolv.conf text -> array of problem strings. Keywords are matched exactly as Resolv and glibc do (lower-case); comment
      # lines (`#`, `;`) and blank lines are skipped, and a trailing comment is not part of an option.
      def self.analyze(text)
        problems = []
        text.to_s.each_line do |raw|
          line = raw.strip
          next if line.empty? || line.start_with?('#', ';')

          keyword, *args = line.split(/\s+/)
          case keyword
          when 'search' then problems << "resolv.conf has a `search` line (#{args.join(' ')})"
          when 'domain' then problems << "resolv.conf has a `domain` line (#{args.join(' ')})"
          when 'options' then problems.concat(options_problems(args))
          end
        end
        problems
      end

      def self.options_problems(args)
        args.each_with_object([]) do |opt, out|
          next unless (m = /\Andots:(\d+)\z/.match(opt))

          out << "resolv.conf sets `options #{opt}` (ndots > 1 reorders resolution toward suffixed names)" if m[1].to_i > 1
        end
      end

      private_class_method :file_problems, :options_problems
    end
  end
end
