---
name: feature-request
description: >-
  Evaluate a feature request against the existing codebase and README, note overlaps with existing functionality, and outline an implementation sketch. Attaches automatically to issues labelled "enhancement".
labels: [enhancement, feature]
---

# Feature request review

Goal: give maintainers enough context to accept, scope down, or decline the request.

## Steps

1. Read the README and any docs directory to learn the project's stated scope.
2. Check whether the requested behaviour already exists, partially exists, or was previously declined (search closed issues).
3. Identify the packages or files that would change.
4. Comment with:

   ```
   ## Feature review
   Summary: …
   Already possible? yes / partially / no (how)
   Fits project scope? yes / unclear / no (why)
   Sketch: 3–6 bullet points naming files or packages
   Open questions: …
   ```

5. Add the `enhancement` label if it is missing.

## Do not

- Do not implement the feature.
- Do not decline on the maintainers' behalf; only surface the trade-offs.
