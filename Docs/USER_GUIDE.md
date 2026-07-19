# User Guide — understanding your verification results

This guide explains what each verification result means, in plain language. It is written
for the person reading a result and deciding what to do with an email address — not for the
engineers who run the system.

You will get one result per address. Each result has four parts:

- a **status** — the headline answer,
- a **reason** — the single most important cause behind that answer,
- a **confidence** — how sure the system is, from 0 to 100,
- a **recommendation** — what we suggest you do next.

The rest of this guide explains each of these, and — most importantly — explains why **"we
can't tell"** is a real, useful answer rather than a failure.

---

## 1. The four statuses

Every address resolves to exactly one of four statuses. There are only four, on purpose:
each one means something specific, and we never blur them.

| Status | What it means | In practice |
|---|---|---|
| **valid** | We have positive evidence the mailbox can receive mail. | Safe to send. |
| **invalid** | We have positive evidence the mailbox cannot receive mail. | Don't send — it will fail. |
| **risky** | The address is real enough to accept mail, but sending carries elevated risk (it may be a shared inbox, a temporary address, or a domain that accepts everything). | Send with care, or review. |
| **unknown** | We could not determine the answer. | See §3 — this is honest, not broken. |

Two things are worth stating plainly:

- **valid means we found evidence *for* delivery, and invalid means we found evidence
  *against* it.** Neither is a guess. If we cannot find evidence in either direction, the
  answer is **unknown** or **risky** — never a coin-flip dressed up as `valid` or `invalid`.
- **We will never call a real address `invalid` just because we couldn't check it.** An
  address we couldn't verify is `unknown` or `risky`. `invalid` is reserved for addresses we
  have actual evidence are undeliverable. This protects you from silently discarding good
  contacts.

---

## 2. The reason — the "why" behind the status

The reason names the single most important cause of the status. You don't need to memorize
the full list; the common ones are self-explanatory:

- **invalid_syntax** — the address isn't a well-formed email address.
- **invalid_domain / no_mx** — the domain can't receive email at all (it has no mail service).
- **mailbox_not_found** — the mail service told us this specific mailbox does not exist.
- **mailbox_full** — the mailbox exists but is currently over its storage limit. (Note this is
  a **valid** result: a full mailbox is a real mailbox — see §5.)
- **role_account** — the address is a shared/functional inbox (like an "info@" or "sales@"
  style address) rather than a person. Delivered, but often lower engagement.
- **disposable** — the address is a throwaway/temporary address. Real today, likely gone
  tomorrow — so it's **risky**, never treated as invalid.
- **catch_all / accept_all** — the domain accepts mail to *every* possible address, so we
  can't confirm this *particular* mailbox exists (see §5).
- **plausible** — the address looks deliverable and we found nothing wrong, but we couldn't
  obtain positive proof either way.

The reason vocabulary is fixed and stable: a reason means the same thing today as it did the
first day the system ran, so you can build your own rules on top of it with confidence.

---

## 3. What "unknown" means — and why it's a good answer

**`unknown` means: we could not obtain reliable evidence about this address, so we are telling
you that honestly instead of guessing.**

It is tempting to want every address sorted cleanly into "good" or "bad." But some addresses
genuinely cannot be determined at the moment we check them — and when that happens, the most
useful thing a verification system can do is *say so*, rather than invent an answer that might
be wrong.

Here's why that matters to you:

- A fake `valid` costs you a bounce, and enough bounces damage your ability to reach *anyone*.
- A fake `invalid` costs you a real customer you'll never contact again.
- An honest `unknown` costs you nothing but a moment's judgment — and it's *recoverable*: you
  can decide to send cautiously, verify again later, or hold.

So `unknown` is not the system giving up. It is the system refusing to gamble with your data.
A verification service that never returns `unknown` is not more capable — it's just guessing
in the cases this system is honest about.

**What to do with an `unknown`:** treat it as "proceed with caution." Many `unknown`
addresses are perfectly deliverable; we simply couldn't prove it in advance. The
recommendation (§6) will guide you.

---

## 4. The confidence number — what it is and isn't

Confidence is a number from **0 to 100** that expresses **how strongly the evidence supports
the status**. Higher means stronger evidence.

**What confidence is:**

- A measure of how much to trust *this specific result*.
- A way to sort or prioritize: a `valid` at 95 is a stronger bet than a `valid` at 72.
- A companion to the status, never a replacement for it.

**What confidence is *not*:**

- It is **not a probability that the mail will be delivered.** A confidence of 80 does not mean
  "80% chance of delivery." It means "the evidence for this status is strong." Delivery also
  depends on things no verifier can see — the recipient's spam filter, whether they've muted
  you, whether their mailbox fills up tomorrow.
- It is **not comparable across different statuses as a single ranking.** A `risky` at 60 and a
  `valid` at 60 are not "equally good" — the status is the headline; confidence refines it.
- It is **not a grade for the address's owner or worth.** It's about verifiability, nothing more.

Read status and confidence *together*: the status tells you the answer, the confidence tells
you how much weight to put on it.

---

## 5. Addresses that cannot be determined — a property of email itself

Some addresses are **inherently undeterminable** — not because of any limitation of this
system, but because of how email works. This is the single most important thing to understand,
because it explains a large share of `risky` and `unknown` results, and it will never fully go
away no matter how good the system gets.

Two situations account for most of it:

### 5.1 Large mailbox providers

The biggest consumer mail providers **deliberately do not reveal** whether an individual
mailbox exists. If you ask them "does this specific address exist?", they answer the same way
whether it does or not — as an anti-abuse measure, to stop people from harvesting valid
addresses. This is by design, on their end.

So for an address at one of these providers, the honest answer is often **risky** with a reason
noting we couldn't get a definitive mailbox check — *not* because we didn't try, but because
the provider does not answer the question for anyone. The address may well be perfectly
deliverable; we simply cannot obtain proof in advance, and we won't pretend otherwise.

### 5.2 Catch-all domains

Some domains are configured to **accept mail addressed to anything** — `anything@theirdomain`
is accepted, whether or not a real person is behind it. These are called **catch-all**
domains.

On a catch-all domain, "the server accepted it" tells you nothing about whether *this specific*
mailbox exists — because the server accepts *every* address. So we mark these **risky /
catch_all**: the mail will likely be accepted, but we can't confirm a real recipient is there.

### The honest framing

In both cases, the limitation is **a property of email**, not a defect in the verification. No
verifier can determine an address the provider refuses to confirm, or single out one real
mailbox on a domain that accepts them all. A system that returns a confident `valid` for these
is overstating what it knows. This one returns `risky` — the truthful answer — and lets you
decide.

---

## 6. The recommendation — what to do next

Alongside the status, each result carries a suggested action. There are three:

| Recommendation | Meaning |
|---|---|
| **send** | The evidence supports delivery. Go ahead. |
| **review** | Worth a human look, or send with care — the result is real but carries some risk or uncertainty. |
| **skip** | Don't send — either proven undeliverable, or the risk outweighs the benefit. |

The recommendation is a convenience: it distills the status and confidence into a single next
step. You are always free to apply your own thresholds — for example, treating some `review`
results as `send` for a warm, engaged audience, or as `skip` for a cold, reputation-sensitive
one. The recommendation follows the status honestly: a `valid` is never told to `skip`, and an
`invalid` is never told to `send`.

---

## 7. Putting it together — reading a result

A worked example, in plain terms:

> **status:** risky · **reason:** catch_all · **confidence:** 45 · **recommendation:** review

Read this as: *"The domain accepts all mail, so we can't confirm this exact mailbox exists.
The address is probably deliverable, but we have no positive proof of the specific recipient.
We suggest you review before sending — especially if this is a cold, reputation-sensitive
campaign."*

And another:

> **status:** valid · **reason:** mailbox_full · **confidence:** 88 · **recommendation:** send

Read this as: *"The mailbox definitely exists — we know because it's currently over quota,
which only a real mailbox can be. It's a safe address; it may just be temporarily full."*

---

## 8. The one principle behind all of it

Everything above comes from a single commitment:

> **We tell you what we can actually prove, and we tell you honestly when we can't prove
> anything.**

That's why there are exactly four statuses, why `unknown` is a first-class answer and not an
error, why confidence measures evidence rather than promising delivery, and why some addresses
are marked `risky` instead of being forced into a confident guess. A result you can trust is
worth more than a result that's merely tidy — and a verifier that admits the limits of what's
knowable is one you can build on.
