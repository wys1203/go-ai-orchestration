---
name: bug-report-review
description: >-
  Investigate an issue labelled as a bug. Locate the relevant code, confirm whether the report is plausible, and write a root-cause hypothesis with file references. Attaches automatically to issues labelled "bug".
labels: [bug]
---

# Bug report review

Goal: turn a bug report into a maintainer-ready analysis. You do not fix the bug in this skill unless the issue explicitly asks for a pull request.

## Steps

1. Restate the expected vs actual behaviour in one line each.
2. Use code search and file reading tools on the default branch to find the code path involved. Cite files with their path and a line range.
3. Check recent commits or pull requests that touched those files; note any that could have introduced the regression.
4. Decide one of:
   - **Confirmed**: the code clearly does what the reporter describes
   - **Likely**: consistent with the code but not proven
   - **Cannot reproduce from code alone**: needs runtime information
5. Comment with:

   ```
   ## Analysis
   Expected: …
   Actual: …
   Suspected cause: … (path:line)
   Confidence: Confirmed | Likely | Unclear
   Suggested fix: … (2–4 lines)
   ```

6. If confidence is Unclear, add the `needs-info` label and state precisely what output or environment detail would settle it.

## Do not

- Do not open a pull request unless the issue text asks for one.
- Do not speculate about code you did not read.
