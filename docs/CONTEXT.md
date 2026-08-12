# docs/

Project documentation beyond code-level CONTEXT.md files.

## Structure

```
docs/
├── CONTEXT.md           # This file
└── stage-plans/
    ├── TEMPLATE.md      # Required format for stage plans
    └── stage-N-*.md     # Individual stage plans (written before each stage)
```

## Stage Plans

Each development stage gets a plan document written **immediately before** that
stage begins. The plan is reviewed before any code is written.

See `stage-plans/TEMPLATE.md` for the required format.

**Naming convention:** `stage-{N}{track}-{short-name}.md`
- `stage-1a-data-schemas.md`
- `stage-2a-go-api.md`
- `stage-3b-local-queue.md`
- etc.

The letter suffix marks parallel tracks within a phase: `1a` and `1b` run at the
same time, as do `3a` and `3b`.

## How to Use

1. Before starting a stage, copy `TEMPLATE.md` to `stage-N-name.md`
2. Fill in all sections
3. Review with user
4. Implement
5. Check off verification items as you complete them

## Superseded Plans

Stage plans are a record of what was decided at the time, not living
documentation. When a decision is reversed, the plan keeps its original content
and gets a `> Superseded:` note at the top pointing at what replaced it. Plans
are not rewritten — a plan edited to match the present tells you nothing about
why the project moved.

Currently superseded:

| Plan | Extent | Superseded by |
|------|--------|---------------|
| `stage-1a-data-schemas.md` | Naming only | `stage-3b-local-queue.md` — same schemas, different transport |
| `stage-1b-local-infrastructure.md` | Entirely | `infra/CONTEXT.md` — Docker, LocalStack and Redis were removed |
| `stage-2a-go-api.md` | Cache, queue, compose | `stage-3b-local-queue.md`, `infra/CONTEXT.md` |

## Non-Obvious Decisions

- **Plans before code:** Prevents wasted effort from misaligned expectations
- **Exact data shapes:** Forces thinking through boundaries before coding
- **Observable outcomes:** Makes "done" unambiguous
- **Plans are append-only:** Corrections go in a superseded note, not an edit
