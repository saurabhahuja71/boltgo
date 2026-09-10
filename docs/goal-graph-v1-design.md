# Bolt Goal Graph v1

## 1. Motivation

Bolt’s current execution engine is a bounded PLAN/ACT/OBSERVE/VERIFY loop for one user turn. It already has durable acceptance criteria, verification criteria, observations, failures, retries, replanning, and a final completion gate.

The final Coding Eval showed that the engine is stable, including the two verification fixes. The remaining weakness is goal progress: a model can inspect without producing a requested artifact, complete some independent requirements and lose track of others, or spend its bounded turns retrying without making the remaining work explicit.

Goal Graph v1 adds a thin goal-oriented coordination layer. It does not replace the current loop, weaken completion, or create a second verification system.

The intended separation is:

- Goal Graph: what remains, what depends on what, and which node is eligible.
- Existing agent loop: how one eligible node is planned, acted on, observed, and verified.
- Existing acceptance and verification state: authoritative completion evidence.

## 2. Current Architecture

The current architecture is centered in `internal/agent`:

- `Agent.RunUserMessage` resets state, builds model context, advertises tools, and runs bounded model/tool rounds.
- Each iteration appends a concise private control context containing the original goal, plan, current step, observations, failures, acceptance state, and verification state.
- Structured model tool calls are sanitized, executed sequentially by the tool registry, and recorded in `AgentRunState`.
- `AgentRunState.addObservation` records the tool call, observation, failure, verification evidence, and acceptance updates.
- `recordVerification` maps `run_tests` and relevant `run_shell` commands to durable `VerificationCriterion` entries.
- `canVerify()` requires all verification criteria to pass and preserves failure-recovery behavior.
- `canComplete()` requires overall verification, all verification criteria, and every acceptance criterion. It is the final gate.
- Empty/model-only responses trigger verification or bounded replanning; tool failures record retries and replan; iteration, tool-call, and retry bounds safely block.
- `SaveSessionPath` persists messages and `AgentRunState`; `LoadSessionPath` restores both and has compatibility behavior for older sessions without control state.
- Tests in `state_test.go`, `agentic_loop_test.go`, and `session_test.go` exercise criteria, verification, retries, bounded execution, and resume behavior.

Current flow:

    user goal
        |
        v
    RunUserMessage
        |
        v
    begin iteration
        |
        v
    model PLAN/ACT response
        |
        +--> structured tool calls --> registry execution
        |                                  |
        |                                  v
        |                         addObservation / evidence
        |                                  |
        +<---------------------------------+
        |
        v
    private verification prompt
        |
        v
    canVerify() and canComplete()
        |
        +--> complete
        +--> replan/retry
        +--> blocked

Goal Graph v1 sits between the user goal and selection of the next current step, and between an iteration result and the next graph update:

    user goal
        |
        v
    decompose + validate
        |
        v
    GoalGraph selects READY node
        |
        v
    existing PLAN -> ACT -> OBSERVE -> VERIFY loop
        |
        v
    evidence projection + graph update
        |
        +--> next READY node
        +--> REPLAN
        +--> existing completion gate

## 3. Proposed Architecture

The proposed architecture has five explicit layers:

1. Goal intake preserves the exact original user goal.
2. Decomposition produces candidate graph nodes and dependencies.
3. Deterministic graph validation rejects unsafe or unusable graphs.
4. Graph execution selects one READY node and supplies its bounded node objective to the existing agent loop.
5. Evidence projection updates the graph from tool results and existing criteria without creating parallel verification semantics.

The graph is an execution map, not a replacement for `AgentRunState`. It answers “what should happen next?” while `AgentRunState` remains authoritative for current-turn tool evidence and the completion gate.

A node is eligible only when:

- it is required;
- it is not already satisfied;
- all required dependencies are satisfied;
- it is not blocked by an unresolved failure;
- the global bounds still permit execution.

A graph-level DONE indication is advisory until the existing acceptance and verification gate also passes.

## 4. Goal Graph Data Model

The minimal conceptual model is:

    GoalGraph
      goal_id
      original_goal
      version
      nodes[]
      root_ids[]
      terminal_ids[]
      current_node_id
      replan_count
      graph_status

    GoalNode
      id
      description
      type
      status
      required
      depends_on[]
      evidence[]
      attempts
      failures[]
      executor
      last_updated

Suggested Go-like shape for design purposes:

    type GoalGraph struct {
        Version       int
        GoalID        string
        OriginalGoal  string
        Nodes         []GoalNode
        CurrentNodeID string
        Replans       int
        Status        GraphStatus
    }

    type GoalNode struct {
        ID          string
        Description string
        Type        GoalNodeType
        Status      GoalNodeStatus
        Required    bool
        DependsOn   []string
        Evidence    []GoalEvidence
        Attempts    int
        Failures    []GoalFailure
        Executor    string
    }

Stable IDs should be deterministic within one decomposed goal. They should be semantic and short, for example `explore-average`, `verify-relevant-test`, and `verify-go-test-all`. If decomposition is regenerated, existing IDs should be retained where the requirement is unchanged.

Evidence should be references or compact summaries, not large tool output:

    type GoalEvidence struct {
        Kind       string
        Source     string
        Summary    string
        Satisfied  bool
        RecordedAt time
    }

The graph must not duplicate full tool transcripts. Existing `ToolCallRecord`, `Observation`, `FailureRecord`, `AcceptanceCriterion`, and `VerificationCriterion` remain the detailed evidence sources.

Recommended graph statuses are:

- `active`: at least one required node remains.
- `complete`: every required node is satisfied and the existing final gate passes.
- `blocked`: no eligible node can progress because of unresolved dependencies or bounds.
- `invalid`: graph validation failed before execution.

Recommended node statuses are:

- `pending`: known requirement, not yet eligible.
- `ready`: dependencies are satisfied and it may execute.
- `running`: selected as the current node.
- `observed`: an attempt produced evidence, but deterministic evaluation is not yet satisfied.
- `satisfied`: required evidence is present.
- `failed`: the last attempt failed but the node may be retried or replanned.
- `blocked`: the node cannot run until a dependency or external condition changes.

`observed` is useful because a model can produce a meaningful inspection result without satisfying an implementation or artifact requirement. It prevents “the model said it is done” from being confused with evidence-backed satisfaction.

## 5. Node Types

V1 should use six types because the benchmark and current state distinguish them materially:

- `exploration`: inspect repository structure, source, or assumptions.
- `implementation`: create or change production behavior, or confirm an already-correct implementation.
- `test_creation`: add or update a test file or test case.
- `test_execution`: run a targeted or full test command.
- `verification`: evaluate required verification criteria and final consistency.
- `artifact`: create a required report, trace, README update, or other named deliverable.

These are labels for decomposition and observability, not separate execution engines. All six use the same existing loop and tools.

A node may have more than one evidence kind. For example, an implementation node may require a source mutation or an explicit no-op inspection plus a passing verification criterion. A test-execution node maps to existing verification criteria rather than introducing a new test-result store.

If implementation and artifact requirements prove indistinguishable in early implementation work, they may share an internal execution path while retaining distinct node types in the graph. They should not be collapsed at the graph contract because artifact omissions were a measured failure pattern.

## 6. Node Lifecycle

The lifecycle is:

    PENDING
       |
       | dependencies satisfied
       v
    READY
       |
       v
    RUNNING
       |
       v
    OBSERVED
       |
       | deterministic evidence predicate passes
       v
    SATISFIED

Failure paths:

    RUNNING  --> FAILED
    RUNNING  --> BLOCKED
    FAILED   --> READY       after retry/replan makes it eligible
    BLOCKED  --> READY       after dependency or condition changes

Rules:

- Only the graph scheduler changes a node to `RUNNING`.
- Tool results first enter existing `AgentRunState`; the graph sees a projection of those results.
- A successful model response alone only permits `OBSERVED`, never `SATISFIED`.
- A node with a failed required verification remains unsatisfied even if another command succeeds.
- Repeating an attempt increments `Attempts`; it does not erase previous failure evidence.
- A satisfied node is immutable for ordinary replanning. It may be invalidated only by explicit contradictory evidence or a changed original goal.

## 7. Dependencies

Dependencies are directed edges from prerequisite node IDs to dependent node IDs. A node is READY when every required dependency is SATISFIED.

Sequential example:

    explore-average
          |
          v
    implement-average
          |
          v
    test-average-relevant
          |
          v
    test-go-all
          |
          v
    final-verification

Multiple prerequisites:

    explore-source ----+
                       |
    inspect-tests -----+--> implement-change --> test
                                                                                         +--> artifact

Independent nodes have no dependencies or share only a later verification dependency. This permits safe parallel scheduling in a future design, but v1 executes one node at a time.

A blocked node reports the unsatisfied dependency IDs. It must not be silently skipped or counted toward completion. Replanning may add a prerequisite or replace a failed node, but it must preserve satisfied nodes and maintain a traceable relationship to the original requirement.

## 8. Goal Decomposition

Three approaches were considered:

### Deterministic rule-based decomposition

Advantages:

- predictable;
- easy to test;
- safe for explicit commands and named artifacts.

Limitations:

- weak for implied relationships and unfamiliar wording;
- can overfit phrases.

### LLM-generated decomposition

Advantages:

- handles natural language and domain-specific work;
- can propose useful exploration and dependency structure.

Limitations:

- may omit requirements;
- may invent unnecessary work;
- may create invalid cycles or disconnected verification;
- cannot be trusted as completion evidence.

### Hybrid recommendation

Use a hybrid:

    original goal
        |
        +--> deterministic extraction of explicit requirements
        |
        +--> LLM proposes node descriptions/dependencies
        |
        +--> deterministic validation and reconciliation
        |
        v
    executable GoalGraph

Deterministic extraction should reuse the existing acceptance and verification parsing concepts. It should identify explicit mutations, test creation, named artifacts, explicit verification commands, and generic final verification. It should not attempt arbitrary semantic equivalence between commands.

The LLM may refine descriptions, add exploration prerequisites, and suggest dependencies. Bolt must retain every deterministic requirement, reject unsupported additions that have no meaningful goal, and require the LLM graph to cover all extracted requirements.

Validation must reject:

- duplicate node IDs;
- empty or meaningless descriptions;
- unknown dependency IDs;
- self-dependencies;
- cycles;
- unreachable required nodes;
- no terminal path for required work;
- a terminal/DONE node that bypasses required nodes;
- verification nodes disconnected from the work they verify;
- a verification node with no observable evidence predicate;
- a required node that can never become READY;
- a graph that silently drops an extracted acceptance or verification requirement;
- a graph that marks a node SATISFIED before execution evidence exists.

A graph can contain independent roots, but every required node must be reachable from the user goal and every required branch must converge on a terminal verification path or be explicitly classified as independently terminal.

## 9. Evidence Model

Evidence is deterministic, typed, and sourced from current Bolt state.

### Exploration

Evidence:

- successful `repo_map`, `list_dir`, `read_file`, `grep`, or `find_files`;
- observation summary showing the requested object was located;
- for assumption-checking, the relevant source observation.

The model’s prose claim is not evidence by itself.

### Implementation

Evidence:

- `write_file` or `str_replace` targeting the required path;
- source observation showing the requested behavior already exists for a legitimate no-op;
- subsequent verification evidence appropriate to the requirement.

A no-op implementation node must support “already satisfied” only when inspection and required verification establish that fact. It must not require an unnecessary source mutation.

### Test creation

Evidence:

- `write_file` or `str_replace` targeting a test path;
- existing test evidence when the original task explicitly permits an existing test to satisfy the requirement;
- the existing `tests_added` acceptance criterion remains authoritative for current completion.

### Test execution

Evidence:

- successful `run_tests` evidence;
- successful matching `run_shell` test evidence;
- the existing `VerificationCriterion` status and command-specific matching.

An empty `run_tests` command continues to mean the configured full test suite. Build or vet evidence cannot satisfy a test-execution node unless the original requirement explicitly asks for build or vet.

### Verification

Evidence:

- all matching existing verification criteria are `passed`;
- `canVerify()` remains the existing authority;
- no new graph-specific verification result is introduced.

### Artifact

Evidence:

- required path exists;
- content predicate is deterministic and tied to the original requirement;
- the mutation or observation is recorded in existing tool history.

The graph should reference the acceptance criterion and its evidence rather than reimplementing `canComplete()`. A node cannot become SATISFIED merely because the final assistant response says “done.”

## 10. Graph Execution

The graph scheduler selects the next READY required node, marks it RUNNING, and constructs a concise node objective. The existing agent loop then executes that objective using its current PLAN/ACT/OBSERVE/VERIFY behavior.

Conceptual flow:

    select READY node
        |
        v
    node-scoped PLAN
        |
        v
    existing ACT / tool calls
        |
        v
    existing OBSERVE / AgentRunState update
        |
        v
    existing VERIFY
        |
        v
    deterministic graph evidence projection
        |
        +--> SATISFIED: select next READY node
        +--> OBSERVED/FAILED: retry or replan
        +--> BLOCKED: expose dependency/failure
        +--> all nodes satisfied: existing canComplete()

Tool mapping is evidence-based:

- read/search tools primarily update exploration nodes;
- mutation tools update implementation, test-creation, or artifact nodes based on target path and original requirement;
- `run_tests` and qualifying `run_shell` calls update test-execution and existing verification criteria;
- nonmatching tools remain observations and do not satisfy unrelated nodes.

The current `AgentRunState.CurrentStep` can temporarily identify the selected graph node plus tool name in a future implementation, but the graph should own node identity. A tool call should not be assigned to a node solely because it occurred while that node was current; the deterministic evidence predicate must also match the node requirement.

Failures update both stores:

- existing state records the tool failure, category, retryability, and observation;
- the graph records a compact node failure and marks the node FAILED or BLOCKED;
- satisfied nodes remain satisfied;
- retry limits remain bounded by existing agent limits and future graph-level limits.

Replanning is graph-aware but loop-compatible. It chooses another READY node, adds a missing prerequisite, or retries a FAILED node. It does not reset the original goal or rebuild all progress from zero.

## 11. Replanning

Replan when:

- a tool fails;
- a test fails;
- an inspection disproves an assumption;
- a dependency becomes blocked;
- a node’s evidence predicate cannot be met with the current plan;
- new information changes the required work;
- the model returns without advancing a required node.

Every replan must preserve:

- the exact original goal;
- all SATISFIED nodes;
- their evidence references;
- prior attempts and failures;
- existing acceptance and verification state.

A replan may:

- add a prerequisite node;
- split an unsatisfied node into smaller nodes;
- change dependencies;
- select a different READY node;
- mark an impossible requirement BLOCKED with an explicit reason.

A replan must not:

- delete evidence;
- mark an unsatisfied node satisfied;
- restart already satisfied work without contradictory evidence;
- create an unbounded chain of replans;
- declare DONE because no current node is running.

The first v1 implementation should prefer deterministic replan actions for tool/test failures and use the LLM only to propose the next graph adjustment. Bolt validates the proposed adjustment before applying it.

## 12. Completion

Current `canVerify()` and `canComplete()` remain unchanged.

The eventual graph-level rule is:

    graph DONE
      iff every required graph node is SATISFIED
      and all existing acceptance criteria are satisfied
      and all existing verification criteria are passed
      and existing canVerify()/canComplete() succeeds

The graph must make missing work explicit; it must not weaken the final gate. If the graph says all nodes are satisfied but `canComplete()` is false, the overall state is blocked/incomplete and the discrepancy is observable.

The graph’s terminal verification node is a coordination node, not a replacement for the existing verification architecture. Its evidence projection must use existing `VerificationCriterion` status and current acceptance state.

## 13. Persistence / Resume

A graph needs durable state for:

- graph version;
- original goal and stable goal ID;
- node IDs, descriptions, types, statuses, dependencies;
- node evidence references;
- attempts and compact failures;
- current node;
- graph replans and status;
- optional executor label.

For minimal disruption, use a separate `GoalGraph` value in the session envelope, with an optional/omitted field for backward compatibility. The current `AgentRunState` should remain the authority for existing criteria, tool calls, observations, failures, verification, and completion.

No current `AgentRunState` change is required for this design document. During implementation, the least disruptive wire-compatible option is an optional `goal_graph` field alongside `run_state`, or an optional pointer field on `AgentRunState` with `omitempty`. The choice should be made only after a persistence prototype and migration tests.

Backward compatibility:

- sessions without `goal_graph` load as legacy single-loop sessions;
- legacy sessions must not be treated as having graph completion;
- resuming a legacy session may create a graph from the original goal only after preserving existing run state;
- malformed graphs fail closed as incomplete, with the existing session still inspectable;
- graph schema versioning allows future fields without changing old sessions.

Resume must restore the current node and all node evidence before another model request. A resumed graph must not rerun satisfied nodes merely because the process restarted.

## 14. Model-Facing State

The model should receive a concise projection, not hidden chain-of-thought and not the entire graph history.

Example:

    Goal progress: 3/6 required nodes satisfied

    Completed:
      ✓ inspect Average
      ✓ confirm implementation
      ✓ add focused test

    Current:
      → run relevant test

    Remaining:
      ○ run go test ./...

    Blocked:
      ○ final verification (depends on run relevant test and go test ./...)

    Node constraints:
      - satisfy the current node with tool evidence
      - preserve the original goal
      - do not claim completion while remaining nodes exist

The projection should include:

- progress counts;
- current node ID and description;
- completed node summaries;
- READY alternatives when useful;
- remaining nodes;
- blocked nodes and dependency reasons;
- the latest compact failure relevant to the current node;
- evidence expectations for the current node.

It should omit:

- chain-of-thought;
- full old tool payloads;
- unrelated graph branches when context is constrained;
- any claim that a node is satisfied unless the deterministic graph state says so.

The existing private control context remains the source for acceptance and verification details. The graph summary is an additional compact status block, not a replacement.

## 15. Observability

Future telemetry should record:

- graph ID and schema version;
- total required/optional nodes;
- satisfied, ready, running, observed, failed, and blocked counts;
- current node ID/type;
- node attempts;
- node start/end times;
- dependency-block count and dependency IDs;
- graph-level replans;
- evidence predicate evaluations and failures;
- time to first progress;
- graph completion time;
- whether final completion was blocked by graph state, acceptance state, or verification state;
- node-to-tool-call mapping;
- preserved original goal hash or ID.

This telemetry should be derived from persisted state, just like the final benchmark review used persisted `run_state`. It should make it possible to distinguish:

- model did not attempt a node;
- tool failed;
- evidence did not match;
- dependency was not ready;
- current gate correctly blocked;
- benchmark checker disagreed externally.

## 16. Task 11 Walkthrough

Original task:

    Inspect the failing behavior in Average, fix Average(-4, -6) to return -5,
    run the relevant test, then run go test ./....

Proposed graph:

    [explore-average]
            |
            v
    [implement-or-confirm-average]
            |
            v
    [verify-relevant-test]
            |
            v
    [verify-go-test-all]
            |
            v
    [final-verification]
            |
            v
           DONE

Nodes:

- `explore-average`: inspect `calc/calc.go` and relevant tests.
- `implement-or-confirm-average`: make the minimal fix, or record that the behavior is already correct after inspection.
- `verify-relevant-test`: satisfy existing `verification:relevant_test`.
- `verify-go-test-all`: satisfy existing `verification:go_test`.
- `final-verification`: reconcile graph state with acceptance and verification state.

If the relevant test fails:

- `verify-relevant-test` becomes FAILED;
- its failure and command remain in evidence;
- `verify-go-test-all` remains PENDING because its prerequisite is unsatisfied;
- the graph replans toward diagnosis/fix;
- completion remains blocked.

If the full test is not run:

- `verify-relevant-test` may be SATISFIED;
- `verify-go-test-all` remains READY or PENDING but unsatisfied;
- final verification and completion remain blocked.

If the model stops after implementation:

- implementation may be SATISFIED;
- both verification nodes remain unsatisfied;
- the graph summary shows the exact remaining commands;
- the existing completion gate remains false.

If the model runs only `go test ./...`:

- `verify-go-test-all` becomes SATISFIED;
- `verify-relevant-test` remains unsatisfied;
- the graph and existing independent verification criteria block completion.

If both tests pass:

- both existing verification criteria become PASSED;
- all required graph nodes can become SATISFIED;
- if acceptance criteria also pass, the existing `canVerify()` and `canComplete()` permit completion.

If Average already returns -5:

- the implementation node records inspection/no-op evidence;
- no unnecessary source mutation is required;
- completion is judged by the actual task requirements and verification evidence, not by whether a source edit occurred.

## 17. Benchmark Failure Mapping

The final evaluation supports the following design requirements:

- Models stopping after inspection: artifact and exploration nodes make the missing write explicit; a successful read cannot satisfy an artifact node.
- Incomplete multi-step execution: dependencies and a remaining-node summary keep later actions visible.
- Losing track of requirements: required node counts and explicit remaining/blocked sections persist across turns.
- Bounded-loop exhaustion: node attempts, progress timestamps, and blocked reasons show whether the loop made progress; replanning can select another READY node.
- Retries/replanning without sufficient progress: a node remains OBSERVED/FAILED until evidence changes, preventing repetition from looking like completion.
- Implementation versus verification distinction: implementation and test-execution nodes are separate while existing verification criteria remain authoritative.
- Artifact requirements being under-observable: artifact nodes have deterministic path/content predicates rather than relying on a generic implementation criterion.
- Dependent steps: prerequisite edges prevent final verification from appearing complete before both requested tests run.

The Task 11 fix specifically demonstrates why separate verification nodes must map to the existing independent verification criteria rather than to one generic “tests passed” node.

## 18. Future Multi-Agent Extension

V1 uses one Bolt agent and one existing tool loop for every node. The data model nevertheless reserves an optional executor label:

    executor = "single-agent"   // v1 default
    executor = "explorer"       // future
    executor = "coder"          // future
    executor = "tester"         // future
    executor = "verifier"       // future

The scheduler should select nodes by graph readiness, not by hard-coded agent identity. A future dispatcher can assign a READY node to an executor while preserving the same node evidence contract and session telemetry.

Multi-agent work must not be introduced into v1. There is no shared mutable workspace protocol, conflict policy, or concurrent evidence merge policy in the current engine. Those are future design concerns.

## 19. Migration Strategy

Migration should be incremental:

1. Keep the current single-loop path unchanged and behind its existing behavior.
2. Introduce graph data types and pure validation functions without invoking them at runtime.
3. Add decomposition behind an opt-in feature boundary and compare graph output with existing acceptance/verification state.
4. Add a graph scheduler that selects a node but still invokes the existing loop unchanged.
5. Project existing observations, mutations, test calls, and verification criteria into graph evidence.
6. Persist the optional graph and test legacy session resume.
7. Enable graph execution for controlled evaluation tasks and compare against the frozen baseline.
8. Only after single-agent stability, design executor delegation.

Every stage needs focused unit tests and replayable fixtures. The old path should remain available for session compatibility and rollback during the migration.

## 20. Risks / Open Questions

- How much deterministic decomposition is sufficient without becoming a general NLP system?
- Should stable node IDs be generated from normalized requirement keys, explicit labels, or both?
- How should a no-op implementation be represented when the task says “fix” but the behavior is already correct?
- What is the smallest reliable artifact content predicate without reproducing benchmark checker brittleness?
- Should graph state be a sibling of `run_state` in the persisted envelope or an optional field inside `AgentRunState`?
- How should a resumed graph handle source changes made outside the session?
- What evidence invalidates an already satisfied node?
- How should graph-level bounds relate to existing iteration, retry, and tool-call bounds?
- How should optional nodes affect progress and completion?
- How should future concurrent agents merge conflicting mutations and evidence?
- How can graph telemetry remain compact enough for the model-facing context?
- How can benchmark checker mismatches remain separate from graph evidence evaluation?

The principal safety rule is unchanged: uncertainty must result in an unsatisfied or blocked node, never an optimistic completion.

## 21. Proposed Implementation Phases

### Phase 1: GoalGraph data model and unit tests

Define graph/node/status/dependency/evidence types and pure validation. Test duplicate IDs, unknown dependencies, cycles, disconnected verification, terminal paths, status transitions, and preservation of satisfied nodes.

### Phase 2: Goal decomposition

Implement deterministic extraction plus an LLM proposal boundary. Validate that every explicit acceptance and verification requirement is represented. Keep decomposition observational or opt-in initially.

### Phase 3: Graph execution integrated with the existing loop

Add READY-node selection and node-scoped context. Invoke the existing PLAN/ACT/OBSERVE/VERIFY loop without changing its tool semantics or completion functions.

### Phase 4: Evidence and dependency evaluation

Project existing mutations, observations, acceptance criteria, and verification criteria into node evidence. Add deterministic status transitions and dependency blocking.

### Phase 5: Persistence and resume

Persist an optional versioned graph, restore it with sessions, support legacy sessions, and test restart/resume without rerunning satisfied nodes.

### Phase 6: Benchmark comparison

Run controlled comparisons against the frozen full-evaluation baseline. Analyze remaining-node awareness, artifact completion, verification sequencing, retries, replans, and bounded-loop exhaustion. Keep checker mismatches separate.

### Phase 7: Multi-agent delegation

Only after single-agent graph execution is stable, add executor assignment and dispatch design. Do not introduce multi-agent execution as part of v1.

