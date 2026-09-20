# Automating updates

`stemma reconcile` reconciles the Git-backed project you run it inside: it
publishes the reviewed branch and proposes lock updates as pull requests, in one
finite run with no service behind it. A scheduler invokes it, whether a
Kubernetes CronJob, a systemd timer, a CI schedule or a person at a terminal.
The reviewed branch stays the only source of truth; the command never resolves
newer releases while publishing.

```sh
cd catalog
stemma reconcile
```

`--root` and `--config` select another project the same way they do for every
command.

## Four layers

1. **The checkout selects the Git repository.** The project's checkout must be
   a clone with history. Its `origin` remote is the repository, and the branch
   `origin/HEAD` points at is the reviewed branch. Nothing in the project says
   where the repository lives.
2. **`stemma.yaml` selects the project and its source-control provider.**
   `spec.reconcile.source_control` names the host that reviews proposals and
   records publication, with the provider's own connection settings.
3. **The environment supplies referenced secrets.** Settings use the same
   `{{ env.VARIABLE }}` expressions as destinations, plus `STEMMA_CACHE_DIR` and
   `STEMMA_STATE_DIR` for the cache and state locations. The environment
   supplies values; it never configures the provider by itself.
4. **The scheduler supplies the checkout, storage, timing and resources.** It
   gets the repository onto disk, mounts the cache and state, and runs the
   command.

## What a run does

1. **Fetch.** Fetch the reviewed branch and every `stemma/` branch from origin.
   The working tree is never touched; the reviewed commit and every proposal
   are checked out into temporary directories.
2. **Apply.** When the reviewed commit differs from the last one applied in full,
   run `apply --offline` for the whole catalog and record the result as the
   `stemma/apply` commit status. Every locked input is verified from the cache
   before any destination is written, so one missing object fails the run before
   it publishes anything. The applied marker moves only when every destination
   succeeded; a partial apply is retried next run.
3. **Update.** Resolve every declared input of the reviewed commit once and keep
   one `stemma/Kind/name` branch per resource whose inputs differ from the lock.
   The branch is regenerated from the reviewed commit with only that resource's
   lock entries changed. The resource and every resource consuming its outputs
   are prepared online from the exact locked observations, then planned offline:
   proof that the cache already holds every byte a merge will need. The commit
   is pushed with a lease, the pull request is opened or updated with the
   before-and-after inputs and the plan, and the verification becomes the
   `stemma/plan` commit status.

The phases fail independently. A stale lock on the reviewed branch fails apply
loudly while the update phase still proposes the fix; the exit status is nonzero
when either phase failed. Commit statuses are summaries; the pull request holds
the plan and the run's logs hold the apply detail.

Further behaviour of the update phase:

- A resource the catalog no longer declares gets a lock-cleanup proposal that is
  validated rather than prepared.
- A [suspended](catalogs.md#suspend-a-resource) resource keeps its reviewed lock
  entries and is neither applied nor proposed; run it locally with a selector.
- A proposal a person closed without merging stays declined until its content
  changes.
- Branches whose resource no longer differs from the reviewed lock, including
  merged ones, are closed and deleted.
- A resource that fails to resolve is reported and its existing proposal is left
  as it is.
- Locked plugins must match the lockfile before any plugin code runs. A plugin
  lock problem fails the update phase; fix it with `stemma plugins update` or
  `stemma prepare` and commit the result. A plugin has to be reachable from a
  fresh checkout: an image, or a tracked path.

## Ownership

A branch stays managed while it is exactly one commit ahead of the reviewed
branch, that commit carries the trailer `Stemma-Managed: reconcile/v1`, and it was
created by the identity the provider derives for its credentials:
the App's bot user, or the token's user, under the host's noreply address. Push
your own commit to take a proposal over: the command never rewrites, closes or
deletes it afterwards. Every push and deletion carries a lease against the tip
the run observed.

## Source control

The Project declares the provider. GitHub and GitHub Enterprise Server are
supported; the API endpoint follows the origin's host.

```yaml
spec:
  reconcile:
    source_control:
      type: github
      config:
        client_id: "{{ env.GITHUB_APP_CLIENT_ID }}"
        installation_id: 12345678
        private_key: "{{ env.GITHUB_APP_PRIVATE_KEY }}"
```

| Setting                                       | Purpose                                                                                                                                                         |
| --------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `client_id`, `installation_id`, `private_key` | A GitHub App: its client ID, the numeric ID of its installation on the repository's owner and the PEM key GitHub issued. Installation tokens are minted per run |
| `token`                                       | A personal access token used instead of an App                                                                                                                  |

The App or token needs contents and pull request write access and commit status
write access. An HTTPS origin is fetched and pushed with the same token; an SSH
origin uses the SSH agent. Locally, a personal token and your usual remote are
enough.

## Cache and state

The cache is disposable and shared by the reviewed branch and every proposal;
a proposal warms it for its own merge. The state directory holds only
`reconcile.json`, the applied marker. Losing it repeats one apply, which
converges on what the destinations already hold. To take over by hand, stop the
schedule and run `stemma apply` from any checkout.

After losing the cache, run `stemma plan` in a checkout of the reviewed branch
with the same cache directory. Frozen runs acquire every locked input from its
recorded observation without resolving anything new, so the next reconcile
applies offline again.

## Scheduling

Run one instance at a time; a Kubernetes CronJob uses `concurrencyPolicy: Forbid`.
The shape is the same everywhere:

- a checkout of the repository, produced by whatever the scheduler already uses
  to clone, in disposable space;
- a large cache volume for `STEMMA_CACHE_DIR` that need not be backed up;
- a small state volume for `STEMMA_STATE_DIR`, which keeps a run from applying
  an unchanged commit again;
- the variables `stemma.yaml` references, from the scheduler's secret store.
