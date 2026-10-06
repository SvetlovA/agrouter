# backup.py

A single-file Python script (about 200 lines) that copies a folder to a USB drive every night from
cron. No dependencies beyond the standard library, no tests, one user (me). It calls a web hook in
two places to report success or failure, each with its own copy of a three-try retry loop.

## Conventions
- Keep it one file; Python 3.12.
- Print errors to stderr and exit non-zero.
