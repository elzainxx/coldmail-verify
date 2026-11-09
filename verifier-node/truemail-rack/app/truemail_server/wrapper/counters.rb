# frozen_string_literal: true

require 'monitor'

# FORK EDIT (Phase 2): process-local since-start counters for GET /v1/stats (§4.4). The counter
# SET is a DIAL; only the additive `counters` shape is fixed now so P4 dashboards attach without
# a contract break. This is a raw feed — NO alerting/health-watcher here (that is SEAM → P4).
# Stateless-box note: these are in-memory, reset on restart; the box persists nothing (P1 §4) —
# the brain is the only durable store. thin is multi-threaded, so guard with a Monitor.
module TruemailServer
  module Wrapper
    class Counters
      KEYS = %i[verify_total smtp_sessions catchall_probes timeouts 5xx_total 4xx_total probe_refused].freeze

      def self.instance
        @instance ||= new
      end

      def initialize
        @mon = ::Monitor.new
        @counts = KEYS.to_h { |k| [k, 0] }
        @started_at = process_clock
      end

      def bump(key, by = 1)
        return unless KEYS.include?(key)

        @mon.synchronize { @counts[key] += by }
      end

      def snapshot
        @mon.synchronize { @counts.dup }
      end

      def uptime_s
        (process_clock - @started_at).to_i
      end

      private

      def process_clock
        ::Process.clock_gettime(::Process::CLOCK_MONOTONIC)
      end
    end
  end
end
