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

**Naming convention:** `stage-{N}-{short-name}.md`
- `stage-1-infrastructure.md`
- `stage-2-api-core.md`
- `stage-3-validate-worker.md`
- etc.

## How to Use

1. Before starting a stage, copy `TEMPLATE.md` to `stage-N-name.md`
2. Fill in all sections
3. Review with user
4. Implement
5. Check off verification items as you complete them

## Non-Obvious Decisions

- **Plans before code:** Prevents wasted effort from misaligned expectations
- **Exact data shapes:** Forces thinking through boundaries before coding
- **Observable outcomes:** Makes "done" unambiguous
