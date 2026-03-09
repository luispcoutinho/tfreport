# tfreport

**tfreport** reads a Terraform plan JSON and prints a human-readable, attribute-level diff table — designed for GitLab/GitHub CI pipeline logs and plan review.

```
+--------+---------------------------------------------------------------------+
| CHANGE | RESOURCE                                                            |
+========+=====================================================================+
| update | azurerm_application_gateway.this                                    |
|        |                                                                     |
|        |   probe:                                                            |
|        |   [+] host:                "www-new.example.com"                    |
|        |       interval:            30                                       |
|        |       name:                "probe-new-http"                         |
|        |       path:                "/health"                                |
|        |       protocol:            "Http"                                   |
|        |                                                                     |
|        |   url_path_map:                                                     |
|        |   [~] name: "pm-http-main"                                          |
|        |           path_rule:                                                |
|        |           [+] backend_address_pool_name:  "bp-new"                  |
|        |               backend_http_settings_name: "bs-new"                  |
|        |               name:                       "pr-new"                  |
+--------+---------------------------------------------------------------------+
```

## Features

- **Attribute-level diff** — shows exactly which fields changed, not just which resources
- **Block array set-diff** — for list-type blocks (probes, listeners, rules…), matches elements by `name` and shows only added `[+]`, removed `[-]`, or changed `[~]` elements; unchanged elements are hidden
- **Nested block recursion** — diffs nested blocks (e.g. `path_rule` inside `url_path_map`) at every depth
- **Sensitive value recovery** — resolves `(sensitive)` values from `planned_values` when the provider marks whole blocks as sensitive but the values are not actually secret
- **Terminal-width-aware** — adapts column widths to the terminal or `$COLUMNS`
- **ANSI coloring** — green for add, red for delete, yellow for update

## Block diff legend

| Prefix | Meaning |
|--------|---------|
| `[+]`  | Element added to block array |
| `[-]`  | Element removed from block array |
| `[~]`  | Element exists in both but has changed fields |
| `  - key: value` | Field value before the change |
| `  + key: value` | Field value after the change |

## Installation

```bash
go install github.com/luispcoutinho/tfreport@latest
```

Or download a pre-built binary from the [releases page](https://github.com/luispcoutinho/tfreport/releases).

## Usage

```bash
# From stdin (typical CI usage)
terraform show -json tfplan | tfreport

# From file
tfreport plan.json

# Write to file
tfreport -out summary.txt plan.json

# Print version
tfreport -v
```

## CI/CD — GitLab example

```yaml
plan:
  stage: plan
  script:
    - terraform plan -out=tfplan
    - terraform show -json tfplan > plan.json
    - tfreport < plan.json

# Or capture to artifact
plan:
  stage: plan
  script:
    - terraform plan -out=tfplan
    - terraform show -json tfplan > plan.json
    - tfreport -out plan-summary.txt plan.json
  artifacts:
    paths:
      - plan-summary.txt
    expose_as: "Terraform Plan Summary"
```

## Building from source

```bash
git clone https://github.com/luispcoutinho/tfreport
cd tfreport
go build -o tfreport .
```

## Differences from tf-summarize

tfreport is a focused fork of [tf-summarize](https://github.com/dineshba/tf-summarize) that:

- Always renders attribute-level details (the `-details` flag becomes the default and only mode)
- Implements a block array set-diff engine — unchanged array elements are suppressed
- Removes the summary-only table, tree, JSON, and HTML output modes
- Removes flags: `-tree`, `-separate-tree`, `-draw`, `-json`, `-json-sum`, `-html`, `-md`, `-details`
