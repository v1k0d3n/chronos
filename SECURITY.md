# Security

Chronos is a proof of concept. It is built to be safe to run — its threat model
and controls are described in [docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md),
including the limitations it still has — but it has not been through an
external review, and you should read that document before deploying it
anywhere that matters.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's private
vulnerability reporting on this repository ("Report a vulnerability" under the
Security tab), or contact the maintainer directly through the address on their
GitHub profile.

Include what you found, how to reproduce it, and what you think the impact is.
You will get an acknowledgement, and a fix or a stated decision, as quickly as
one person maintaining a proof of concept can manage.

## What counts

Anything that lets a user of Chronos see a change they could not have read
directly, recover a redacted value, alter or delete a record, or make a change
through a revert that they could not have made themselves. Also anything that
lets Chronos's own ServiceAccounts be used beyond their stated roles.
