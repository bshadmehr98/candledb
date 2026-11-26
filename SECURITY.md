# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub: on the repository's **Security** tab,
choose **Report a vulnerability**. Include what you found, how to
reproduce it, and which version or commit you tested.

You should get a first answer within a week. Once a fix is released, the
report is published with credit to you, unless you prefer otherwise.

## Supported versions

Only the latest release receives security fixes.

## Scope

In scope: anything that lets a client read or write data the security
rules forbid, bypass authentication, obtain the admin key or another
user's tokens, or crash or stall the server with a small number of
requests.

Out of scope: problems that require the admin key, the JWT secret or
access to the server's file system, and the consequences of running with
`-dev-open-rules`, which disables access control by design.
