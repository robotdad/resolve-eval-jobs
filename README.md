# resolve-eval-jobs
Resolve Smart Tool evaluation: Go job service, developed through hosted handoffs and PR delivery

# resolve-eval-jobs

A small, deterministic Go service for local word-count jobs.

## Overview

This service provides a loopback-only JSON HTTP API for submitting and querying word-count jobs. Jobs are processed by a single in-process FIFO worker goroutine. All state is held in memory and is lost on process restart.

## Requirements

- Go 1.22 or later (standard library only, no external dependencies)

## Building

```sh
go build -o word-count-service .
```

## Running

```sh
./word-count-service
```

The service listens on `127.0.0.1:8080` (loopback only). It does not bind to external interfaces.

## API

### POST /jobs

Submit a new word-count job.

**Request body** (JSON):

```json
{"text": "hello world"}
```

- `text` (string, required): the text to count words in. Empty string is valid.
- Unknown fields are rejected with HTTP 400.

**Response** (HTTP 202 Accepted):

```json
{"id": "1"}
```

**Error responses:**

- `400 Bad Request` — malformed JSON, missing `text`, non-string `text`, or unknown fields.

### GET /jobs

List all jobs in submission order.

**Response** (HTTP 200 OK):

```json
[
  {"id": "1", "status": "succeeded", "text": "hello world", "result": {"word_count": 2}}
]
```

### GET /jobs?limit=N (paginated)

Paginate jobs ordered by text ascending (Go bytewise lexicographic), then by numeric ID ascending. Returns a JSON object envelope instead of a bare array.

**Query parameters:**

- `limit` (positive integer, 1–100, required): maximum number of jobs per page.
- `cursor` (opaque string, optional): continuation token from a previous response's `next_cursor`. Requires `limit`.

**Response** (HTTP 200 OK):

```json
{
  "jobs": [
    {"id": "4", "status": "succeeded", "text": "", "result": {"word_count": 0}},
    {"id": "5", "status": "succeeded", "text": "A", "result": {"word_count": 1}}
  ],
  "next_cursor": "<opaque string or null>"
}
```

- `next_cursor` is `null` when there are no further pages.
- When `next_cursor` is non-null, pass it as `cursor` in the next request to continue.

**Error responses:**

- `400 Bad Request` — `limit` is missing, non-positive, non-integer, or exceeds 100; or `cursor` is supplied without `limit`; or `cursor` is malformed.

**Ordering:**

Jobs are sorted by text ascending (Go bytewise / UTF-8 lexicographic), breaking ties by numeric job ID ascending. Identical text values are always returned in ID order.

**Example traversal:**

```sh
# Page 1
curl "http://127.0.0.1:8080/jobs?limit=3"
# {"jobs":[...],"next_cursor":"<token>"}

# Page 2
curl "http://127.0.0.1:8080/jobs?limit=3&cursor=<token>"
# {"jobs":[...],"next_cursor":null}
```

### GET /jobs/{id}

Retrieve a single job by ID.

**Response** (HTTP 200 OK):

```json
{"id": "1", "status": "succeeded", "text": "hello world", "result": {"word_count": 2}}
```

**Error responses:**

- `404 Not Found` — unknown job ID.

### Job lifecycle

Jobs transition through: `queued` → `running` → `succeeded`.

Reads do not consume or mutate jobs. Results are stable once a job succeeds.

## Counting rules

- Words are nonempty runs of non-whitespace characters.
- Whitespace is defined by Unicode (space, tab, newline, U+00A0 non-breaking space, etc.).
- Punctuation is retained within words (e.g., `don't` counts as 1 word, `hello,` counts as 1 word).

Examples:

| Input | Word count |
|-------|------------|
| `""` | 0 |
| `" \t\n"` | 0 |
| `"hello, world!"` | 2 |
| `"don't stop"` | 2 |
| `"one-two"` | 1 |
| `"alpha\u00a0beta"` | 2 |

## Ordering and identity

- Jobs are returned by `GET /jobs` (no `limit`) in submission order.
- Jobs are returned by `GET /jobs?limit=N` in text-ascending, ID-ascending order (see paginated endpoint above).
- Job IDs are distinct, sequential integers, scoped to the process lifetime.
- Submitting identical text creates distinct jobs with distinct IDs.

## Restart behavior

All jobs and results are ephemeral. On process restart:

- `GET /jobs` returns an empty array.
- Any previously known job ID returns HTTP 404.
- The ID sequence restarts from 1.

Cross-process ID uniqueness is not guaranteed and is outside scope.

## Local-only operation

The service binds only to `127.0.0.1` (loopback). It has no external service dependencies, no subprocess execution, and no arbitrary job-command interface. Word counting is the sole job type.

## Administrative CLI

The `jobs-admin` command is a read-only administrative client that lists jobs from the running service.

### Building the CLI

```sh
go build -o jobs-admin ./cmd/jobs-admin
```

### Running the CLI

```sh
./jobs-admin [--endpoint URL] [--json]
```

**Flags:**

- `--endpoint URL` — service base URL (default: `http://127.0.0.1:8080`)
- `--json` — emit a JSON array to stdout instead of human-readable text

### Default (human-readable) output

```
ID      STATUS      WORD COUNT
------  ----------  ----------
1       succeeded   2
2       succeeded   3
```

When no jobs exist:

```
No jobs found.
```

### JSON output

With `--json`, the command emits exactly one valid JSON array to stdout whose content and element order match `GET /jobs`. An empty inventory emits `[]`.

```sh
./jobs-admin --json
# [{"id":"1","status":"succeeded","text":"hello world","result":{"word_count":2}}]
```

### Endpoint selection

By default the CLI connects to the service at `http://127.0.0.1:8080`. Use `--endpoint` to target a different address:

```sh
./jobs-admin --endpoint http://127.0.0.1:9090
```

### Error behavior

- Invalid or non-absolute endpoint URL → exit 1, useful message on stderr
- Unreachable service → exit 1, useful message on stderr
- HTTP error response → exit 1, useful message on stderr
- With `--json`, stdout is always empty on failure; diagnostics go to stderr only

The CLI is read-only: it never creates or mutates jobs.

## Running tests

```sh
go test -v ./...
```

Tests cover:

- Unit tests for counting semantics (all fixed examples verified with literal expected counts)
- HTTP-level acceptance tests (submission, retrieval, counting via HTTP)
- Lifecycle and FIFO ordering (controlled tests, not timing-dependent polling)
- Stable results (three further reads after success)
- Ordered listing (submission order preserved)
- Input rejection (malformed JSON, missing field, wrong type, unknown field → 400)
- Lookup errors (unknown ID → 404)
- Restart boundary (fresh server = empty state)
- Loopback-only and no-subprocess source review tests
- Concurrent submission stress test
- CLI acceptance tests (build, populated/empty inventory text and JSON, error scenarios, read-only behavior)
- Pagination acceptance tests (oracle sequence with limit=1/3/10, tie-boundary cursor correctness, envelope shape, invalid parameter rejection, read-only invariant, race-detector clean)
