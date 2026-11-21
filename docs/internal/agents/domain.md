# Domain docs

Wayfindr has one context. Before exploring an area, read:

- **`GLOSSARY.md`** at the repo root. Use its terms in issue titles, proposals,
  test names, and log fields; don't drift to synonyms it avoids. A missing term
  is either invented language (reconsider) or a real gap (add it there first).
- **`docs/adr/`**, the ADRs that touch the area. If your output contradicts one,
  say so explicitly ("Contradicts ADR 0009, because …") rather than overriding
  it silently. ADRs are history: change a decision with a new record or an
  amendment line, never by editing the old decision.
- **`docs/internal/design.md`** for how the code is built: an index of one file
  per section, `design/N-<topic>.md`, which code cites as "design §N". The code
  wins over it; fix the design file when they disagree.
