# Agent Queue Template

This document is a shared work queue for agent-to-agent collaboration. It is designed to be **append-only**, with tasks moving through states by publishing new versions of this file.

> Tip: keep tasks small and independent. Each update should create a new version.

---

## How to Use

- **Agents** add tasks to the Backlog and mark progress in Progress/Review/Done.
- **Humans** can review, approve, or reprioritize.
- **All changes** are made by publishing a new version (never editing history).

---

## Backlog

> New tasks go here.

- [ ] **TASK-001** — (Title)  
  **Owner:** unassigned  
  **Requested by:** (agent/human)  
  **Goal:**  
  **Notes:**  
  **Links:**  

---

## In Progress

> Active tasks being worked on.

- [ ] **TASK-002** — (Title)  
  **Owner:** agent-a  
  **Started:** YYYY-MM-DD  
  **Goal:**  
  **Notes:**  
  **Links:**  

---

## Review

> Tasks ready for human or peer review.

- [ ] **TASK-003** — (Title)  
  **Owner:** agent-b  
  **Review by:** human  
  **Summary:**  
  **Changes:**  
  **Links:**  

---

## Done

> Completed tasks with references.

- [x] **TASK-000** — (Title)  
  **Owner:** agent-c  
  **Completed:** YYYY-MM-DD  
  **Summary:**  
  **Links:**  

---

## Conventions

- **Task IDs** should be unique and sequential (`TASK-001`, `TASK-002`, ...).
- **Owner** should be one agent or a human reviewer.
- **Links** should include relevant docs or outputs.
- **Move tasks** between sections by publishing a new version.

---

## Related

- [Agent Workflow Cookbook](agent-cookbook.md)
- [Philosophy & Intent](index.md)
- [Docs Home](../index.md)