# gh-notification-cleanup

A [GitHub CLI](https://cli.github.com) extension that marks notifications as **done** when they are no longer
actionable:

- pull requests that were **merged**,
- pull requests that were **closed** without merging,
- **CI failures** (`ci_activity` notifications whose title says the run failed).

Everything else in your inbox is left alone. Unread and read notifications are both handled; notifications that are
already done are not.

## Install

```sh
gh extension install Edholm/gh-notification-cleanup
```

From a local checkout:

```sh
go build -o gh-notification-cleanup . && gh extension install .
```

## Usage

```sh
gh notification-cleanup --dry-run   # show what would be cleaned up
gh notification-cleanup             # clean up merged, closed and CI failure notifications
gh notification-cleanup --merged    # only merged pull requests
gh notification-cleanup --closed --ci
```

| Flag              | Description                                             |
| ----------------- | ------------------------------------------------------- |
| `--merged`        | Notifications for merged pull requests.                 |
| `--closed`        | Notifications for pull requests closed without merging. |
| `--ci`            | Notifications for failed CI runs.                       |
| `-n`, `--dry-run` | Print what would be marked as done, change nothing.     |
| `--verbose`       | Log debug output.                                       |

Without `--merged`, `--closed` or `--ci`, all three are cleaned up. Marking a notification as done cannot be undone.
