# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub private vulnerability
reporting: open the repository's **Security** tab and choose **Report a
vulnerability**. Do not open a public issue or pull request for a security
problem.

Include what you ran, what you expected, what happened, and the version
(`code-index version`). A repository, config file or index that triggers the
problem is the most useful thing to attach.

## Supported versions

Only the latest release (and the current `main`) receives security fixes.

## Scope

code-index reads a repository's files and runs the language servers and
extractors you configure. In scope: a repository's own files (its
`.Wammy/config.json`, its `.Wammy/code.sqlite`, its source and `.gitignore`)
making the tool run a command you did not configure, send data to a host you did
not choose, or read files outside the repository. See "Privacy and security" in
the README for the trust model.
