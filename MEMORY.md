# MEMORY.md

Long-lived engineering rules distilled from prior sessions. This file is
deliberately kept short: only durable, broadly-reusable rules. PR-specific
implementation details, credentials, secrets, and review transcripts
do not belong here.

## Engineering rules

1. **Verify external contracts against primary documentation.** When
   the behavior is security-sensitive, unfamiliar, uncertain, or likely
   to have changed, consult the current primary documentation instead
   of relying on memory. Routine language and standard-library behavior
   does not need repeated research. The cost of a wrong assumption on
   a security-sensitive integration is far higher than the cost of a
   five-minute verification.

2. **Add focused contract tests for external integrations.** Test the
   request fields and behavior that would actually break the
   integration (path, body shape, header values that are part of the
   contract). Avoid testing irrelevant details like header ordering or
   JSON field ordering.

3. **Security hardening must be reviewed together with required
   functionality.** A restriction that prevents the service from
   performing its intended job is not useful hardening; it is a bug.
   When adding `RestrictAddressFamilies`, `MemoryDenyWriteExecute`,
   seccomp filters, capability bounding sets, or similar directives,
   verify they do not break the network calls, TLS, or syscalls the
   service actually needs.

4. **Do not document configuration fields, defaults, guarantees, or
   security properties that the implementation does not provide.**
   Unimplemented claims in docs are worse than no docs at all: they
   mislead future readers and create expectations the code cannot
   meet. If a field, default, or guarantee is documented, the
   implementation must back it up.

5. **Before completing security-sensitive work, perform a focused
   consistency check.** Verify the changed code, tests, configuration,
   and documentation agree. Common mismatches: documented defaults that
   the code does not apply, claimed behaviors that no test exercises,
   tests that assume a value the implementation never produces.

6. **Use least privilege for permissions.** Requested permissions
   should be both necessary for the work and actually available from
   the external system. Granting a permission the upstream does not
   support is a denial-of-service against yourself; granting one the
   caller does not need widens the blast radius of a compromise.