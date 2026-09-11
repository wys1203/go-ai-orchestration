---
name: issue-triage
description: >-
  Classify a new issue (bug / feature / question / invalid), apply the matching labels, and post a short acknowledgement comment. Use for any issue that has not been triaged yet.
---

# Issue triage

Goal: leave the issue in a state where a maintainer can act on it in under a minute.

## Steps

1. Read the full issue and all comments with the GitHub tools.
2. Decide exactly one category:
   - `bug` – something that used to work or is documented to work, does not
   - `enhancement` – a request for new behaviour
   - `question` – a usage question; no code change requested
   - `invalid` – spam, empty, or duplicates an existing open issue
3. If the report is a bug but is missing reproduction steps, version, or environment, add the `needs-info` label and ask for the missing pieces in one comment. List the exact fields you need.
4. Apply the category label. Do not remove labels a human added.
5. If it duplicates another issue, say which one in the comment and add `duplicate`. Do not close it.
6. Post one comment, at most 6 lines, in this shape:

   ```
   Triage: <category>
   <one sentence on why>
   <next step for the maintainer or reporter>
   ```

## Do not

- Do not close issues.
- Do not assign people.
- Do not promise a timeline.
