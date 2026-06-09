# PR Review & Improvement Plan

## Overview
This document outlines planned improvements, refactoring tasks, and documentation updates for the sqd-go-v2 project.

---

## 1. CLI & Configuration Improvements

### 1.1 Replace `noResume` with `--restart` flag
- [ ] Change boolean `noResume` parameter to explicit `--restart` CLI flag
- [ ] Update all call sites
- [ ] Add to help documentation

### 1.2 Use Mustache Templates for Code Generation
- [ ] Replace hardcoded strings in codegen with mustache templates
- [ ] Template files location: `internal/codegen/templates/`
- [ ] Improves maintainability and allows user customization

---

## 2. Code Generation & Templates

### 2.1 Custom Schema & Processor Templates
**Current Status**: Templates exist but need refinement

**Tasks**:
- [ ] Ensure custom processor default has comprehensive comments
- [ ] Add example of:
  - Event parsing
  - State save operation
  - Event array transformation
- [ ] Verify minimal Uniswap template is working
- [ ] Ensure example is correct and well-documented

**Template Structure**:
```go
// custom_schema.go - User-defined entities
// custom_processor.go - Event handling logic
```

### 2.2 Move Init Logic
- [ ] Move `init()` function logic into explicit registration
- [ ] Keep as working example
- [ ] Ensure `sqd-go template` command still works
- [ ] Document registration pattern

---

## 3. Function Naming & Simplification

### 3.1 Clarify Pipeline Functions
**Current confusion**: `runStartPipelineInternal` vs `runDev`

**Tasks**:
- [ ] Add clear function descriptions
- [ ] Rename for clarity if needed
- [ ] Simplify call graph
- [ ] Document when to use each function

### 3.2 Add Function Documentation

**Missing documentation**:
- [ ] `CompactionPruneState` - Add description of purpose and behavior
- [ ] Any other undocumented public functions

---

## 4. Architecture Changes

### 4.1 ReplayBuffer Conditional Logic
- [ ] Only initialize ReplayBuffer when: `currentBlock > finalizedBlock`
- [ ] Finalized block comes from `sqd` request header
- [ ] This saves memory when not needed

### 4.2 Make Cold Cache Default
- [ ] Enable v2 cold cache as default mode
- [ ] Document how to opt-out if needed
- [ ] Update configuration examples

### 4.3 Separate PR for Data Cleanup
**Create separate PR for**:
- v1 and v3 big data chunks
- Unused benchmarks
- Legacy tests

**Reasoning**: Keeps the v2 PR focused on core functionality

---

## 5. Documentation

### 5.1 Wiki Documentation for v2
- [ ] Write short, precise docs
- [ ] Cover:
  - Quick start
  - Custom processor usage
  - State management
  - Configuration options
  - Cold cache mode

### 5.2 Code Comments
- [ ] Ensure all exported functions have godoc comments
- [ ] Add examples for complex operations
- [ ] Document performance considerations

---

## 6. Testing

### 6.1 Validate Examples
- [ ] Run `examples/polymarket/` tests
- [ ] Verify Uniswap PNL example works end-to-end
- [ ] Test custom schema generation

---

## Priority Order

1. **High Priority** (Before merging v2):
   - ReplayBuffer conditional logic
   - Cold cache default
   - Function documentation

2. **Medium Priority** (Can be follow-up):
   - Mustache templates
   - Function renaming/simplification
   - Wiki documentation

3. **Low Priority** (Separate cleanup PR):
   - v1/v3 data removal
   - Unused benchmarks

---

## Notes

- Keep changes minimal and focused
- Each bullet should be a separate commit where possible
- Test after each significant change
- Update this file as tasks are completed
