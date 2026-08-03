---
description: Documentation and comments style
applyTo: '**/*.md, **/*.txt, docs/**, .github/instructions/**, .github/tasks/**, .agents/**, .vscode/skills/**, .opencode/skills/**'
---

- Use direct, terse and specific language
- Do not use amplification, elaboration, and reinforcements except when they are specifically defined (meaning if you use one, you also must write the definition like for programming language variables, but it also must match normal speech use case)
  - 'Important', 'Significant' are meaningless by default
  - 'Critical' means deciding or judging; usually it's better to just say what exactly should be decided
  - 'Necessary' and 'Sufficient' are clear but can be usually omitted
  etc.
- Skip introductions:
  - 'Note that we use ' → 'use ...' or even '...'
  - 'Please note that ' → ''
  - 'In order to ' → 'to '
  - 'is responsible for ' → 'handles ' / 'does '
  - 'It is important to note that ' → ''
  etc.
- Skip adjectives (except when their omission inverses meaning) and fillers
- Add examples from or for the codebase, never generic
- Instead of word Warning, Use ⚠️
- Use → and ← for implications and sequences
- Use [ ] and [x] for checklist items which are getting updated in place
- Do not use other emoticons or emoji unless explicitly requested

# Formatting

- Use → and ← for implications and sequences
- Use [ ] and [x] for checklist items which are getting updated in place
- Do not use other emoticons or emoji unless explicitly requested
- **Item Headers**: Do not add/remove if you see headers like in this item, except when they represent a *scannable* word, and the item does not repeat them:
  - **Meaning**: TLDR of the item
  - **`code_identifier`**: obvious
(if you'll find other use cases, propose it to the user, but do not use without the explicit permission; if the user finds it's a good another exception, they may choose to add it to the list)
