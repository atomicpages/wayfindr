# Triage labels

The skills name five triage roles. In this repo each is a kata label of the same
name, plus `needs-review` for work that isn't done yet.

| Label             | Meaning                                                       |
| ----------------- | ------------------------------------------------------------- |
| `needs-triage`    | A maintainer needs to evaluate the issue.                     |
| `needs-info`      | Waiting on the reporter for more information.                 |
| `ready-for-agent` | Fully specified; an agent can do it unattended.               |
| `ready-for-human` | Needs a person to implement or decide.                        |
| `wontfix`         | Will not be done. Close it with `kata close <ref> --wontfix`. |
| `needs-review`    | Work stopped short of done; the comment says what remains.    |

- Add: `kata label add <ref> <label>`. For `needs-review`, say what remains:
  `kata label add <ref> needs-review --comment "<what remains>"`.
- Remove: `kata label remove <ref> <label>`.
- Filter: `kata list --label <label> --agent`; `kata labels` counts them.

kata creates a label on first use.
