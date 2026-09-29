# Fleet mode

Every chapter before this one describes one host watching itself. Fleet mode
is the optional layer on top: one trinetra becomes the **master** for a
group of others, each of which keeps monitoring and alerting exactly as it
always did, while also shipping a copy of its history to the master and
letting the master re-decide how its alerts get delivered. Nothing about
fleet mode changes what a solo install does, and a fleet host that loses its
link to the master just keeps being a solo host until the link comes back.

This chapter is the guided tour: what the pieces are, how to stand a fleet
up, and how to run one day to day. The deep-dive chapters it keeps pointing
at are where the mechanism lives:

- [Architecture: Fleet mode](02-architecture.md#fleet-mode) -- roles, PKI and
  enrollment, the outbox/shipper data path, gap repair, liveness.
- [Alerting and notification channels: Fleet
  alerting](06-alerting-and-channels.md#fleet-alerting) -- the pipeline,
  handoff/fallback, routing/escalation JSON, silences, grouping,
  dependencies, aggregate rules, managed config.
- [The web UI: Fleet](08-web-ui.md#fleet) -- every fleet page, field by
  field.
- [Command reference: trinetra fleet](11-command-reference.md#trinetra-fleet)
  -- every flag on every `fleet` subcommand.

## Concepts

| Term | What it means |
|---|---|
| **solo** | The default role. No fleet listener, no fleet directories, no behaviour change. |
| **master** | A solo host turned into a fleet hub with `fleet init`. Runs a TLS listener, enrolls children, stores a replica of each one's history, and makes fleet-wide alerting decisions. Shows itself as the node `self`. |
| **child** | A solo host turned into a fleet member with `fleet join`. Keeps sampling, storing, and alerting locally; additionally ships its data to the master and can hand alert delivery off to it. |
| **node** | Any member of the fleet, master included. `trinetra fleet nodes` lists them all. |
| **replica** | The master's per-node copy of a child's history, kept in sync by the outbox/shipper. What `/n/<id>/...` pages and a remote `fleet nodes` row read from. |
| **incident** | One or more related alerts (grouped by rule/severity, node, or tag) tracked as a single unit with its own timeline, acknowledgement, and delivery state. |
| **route / policy** | A route matches an alert (by tag, node, rule, severity) and picks a policy; a policy is an escalation schedule (steps, repeat, whether recovers are sent). |
| **silence / maintenance window** | A silence mutes matching alerts between two times; a maintenance window is a silence that recurs on a weekday/time-range schedule. |
| **managed config** | A small, closed set of config keys the master can push to children by tag, so a tuning change does not mean editing every host by hand. |
| **lease / handoff / fallback** | The mechanism that lets the master take over delivering a child's alert, and the child's guarantee that it delivers the alert itself if the master doesn't. |

## Setting up a fleet

Three commands, run on two kinds of host, get a fleet from nothing to
reporting:

```bash
# 1. On the host that will be the master
sudo trinetra fleet init --address monitor.example.com,203.0.113.7
sudo systemctl restart trinetra
sudo trinetra fleet token create --tags prod --uses 3

# 2. On each server you want to enroll, using the code fleet token create printed
sudo trinetra fleet join swj1_...
sudo systemctl restart trinetra

# 3. Back on the master, confirm they showed up
sudo trinetra fleet nodes --tag prod
```

`fleet init` generates a private CA and a server certificate for the
addresses you gave it, and turns this host into the master -- see [Fleet
mode: Enrollment](02-architecture.md#enrollment) for exactly what gets
created and why the master must terminate TLS itself (no reverse proxy in
front of the fleet port). `fleet token create` mints a short-lived,
limited-use join code that bakes in the master's URL, the token, and a pin
on the CA's public key, so a child never has to trust-on-first-use: it
refuses to send anything unless the certificate the master presents matches
that pin. `fleet join` spends the code, generates a key that never leaves
the child, and gets back a 90-day client certificate. Both `init` and `join`
only write config and certificate files -- they tell the daemon what to
become on its next start, which is why each one ends with `Restart to
apply`.

Everything after that -- nodes, incidents, silences, routing, managed
config -- is a live call to the running master over the control socket, no
restart required.

## Touring the web UI

If `web.enabled` is on, signing in on the master gets you a fleet layer on
top of the regular per-host UI (see [The web UI](08-web-ui.md) if you
haven't set that up yet). `/fleet` is the overview: a health strip you can
click to filter, a heatmap and top-N panels for whichever metric you pick,
and a node table that is sortable, filterable, and pollable in place.

![Fleet overview](../assets/screenshots/fleet-overview.webp)

A dropdown in the top bar (or **Ctrl/Cmd-K** for a fuzzy palette) switches
you to the same page type on a different node -- from one node's history
page to the next node's history page, not its dashboard. Every per-server
page you already know is mounted a second time under `/n/<id>/...` for
whichever node you land on, with a small banner naming its replica state.

![A node's dashboard, reached through the switcher](../assets/screenshots/node-dashboard.webp)

Tick two or more nodes in the `/fleet` table (or follow a tag link) and open
**Compare** to overlay one metric across up to 10 nodes, or view an
aggregated series across a whole filtered set, on the same chart the rest of
the UI uses.

![Fleet compare](../assets/screenshots/fleet-compare.webp)

The rest of this chapter walks the fleet-only pages in the order you would
actually use them; see [The web UI: Fleet](08-web-ui.md#fleet) for the
complete page-by-page reference.

## Alert handoff and local fallback

A child always detects its own alerts, exactly as it would solo. What
changes in fleet mode is *who delivers them*. While the child holds a fresh
lease from the master (renewed every 30s), a firing alert is routed to the
master instead of delivered locally, and the master runs it through fleet
routing, silences, grouping, and escalation before it reaches a channel. If
the master never confirms delivery -- no receipt within
`fleet.fallback_after` (default 2 minutes), or the lease itself expired --
the child delivers the alert itself, prefixed `via local fallback: master
unreachable`, so you can always tell which side actually sent a given
message. A **revoked** child is a different story and says so plainly: see
[Fleet alerting: Lease, receipt, and local
fallback](06-alerting-and-channels.md#lease-receipt-and-local-fallback) for
the exact wording and timing, including what a prolonged outage does (a
local "fleet link down" warning after `fleet.link_down_warn_after`, default
10 minutes).

The practical upshot: nothing you have configured for local channels stops
working when you join a fleet. Fleet routing decides *where the master's
copy* of an alert goes; the child's own local channels remain the backstop.

## Incidents

The master groups related alerts into **incidents** rather than paging once
per alert. List and inspect them from the CLI:

```bash
trinetra fleet incidents --state firing
trinetra fleet incident 4f2a9c1b0d3e
trinetra fleet ack 4f2a9c1b0d3e
```

or from `/fleet/incidents`, which self-polls and is filterable by state,
node, and tag in the URL, and `/fleet/incidents/<id>` for the full detail:
every member alert (with its own delivery/silence state) and the complete
timeline (fired, grouped, delivered, escalated, acked, resolved -- who and
when).

![Fleet incidents](../assets/screenshots/fleet-incidents.webp)

![Incident detail](../assets/screenshots/incident-detail.webp)

Any signed-in role can read both pages; an admin (or the master's Telegram
chat, via the **Ack** / **Silence 1h** buttons on the fire message) can
acknowledge or silence from either. When a node's `depends_on` parent is
down, its own node-down alert folds into the parent's incident instead of
opening a new one -- set that with `trinetra fleet node depends <node>
dep1,dep2,tag:t`.

When you need to know exactly *why* an alert landed where it did --
suppressed, grouped, or delivered to a specific channel -- `fleet explain`
prints the pipeline trail it took:

```bash
trinetra fleet explain cpu:high
trinetra fleet explain 4f2a9c1b0d3e
```

See [Fleet alerting: The pipeline](06-alerting-and-channels.md#the-pipeline)
for what each stage in that trail means, and [Fleet alerting:
Grouping](06-alerting-and-channels.md#grouping) and
[Dependencies](06-alerting-and-channels.md#dependencies) for the grouping
and dependency-fold rules in full.

## Routing and escalation policies

A **route** picks a **policy** for an alert (matched by tag, node, rule, or
severity, first match wins, always a default); a **policy** is an ordered
list of `{after, channels}` escalation steps, with an optional
`repeat_every` and a `send_resolved` toggle. Test a hypothetical alert
against your current config without actually firing one:

```bash
trinetra fleet route test --node web1 --tag prod --rule cpu --severity critical
```

`/fleet/alerting` is the same thing as a page: a structured routes/policies
editor plus a raw-JSON escape hatch, a live aggregate-rules table next to
it, and the same route tester as a form. Saves carry the config's version,
so a concurrent edit is caught as a conflict rather than silently
overwritten.

![Routes](../assets/screenshots/alerting-routes.webp)

![Policies](../assets/screenshots/alerting-policies.webp)

See [Fleet alerting: Routes and escalation
policies](06-alerting-and-channels.md#routes-and-escalation-policies) for
the full JSON schema and a worked multi-route example, and [Aggregate
rules](#aggregate-rules) below for the rule grammar.

## Silences and maintenance windows

A **silence** mutes matching alerts between a start and end time; a
**maintenance window** is a silence that recurs on a weekday set and a daily
time range in a named time zone:

```bash
trinetra fleet silence add --match tag=web,rule=cpu* --for 2h --comment "known noisy deploy"
trinetra fleet silence list
trinetra fleet silence expire <id>

trinetra fleet maintenance add --name "weekly backup window" \
  --match tag=backup --days sat,sun --from 22:00 --to 02:00 --tz Asia/Kolkata
```

`/fleet/silences` lists both, active and upcoming, and lets an admin create
either from the same matcher fields as the CLI's `--match`. Every time on
that page is shown in the master's own local time zone with its
abbreviation, never bare UTC.

![Silences](../assets/screenshots/silences.webp)

The one rule worth memorizing: `node=` in a matcher matches the node's
current display **name as a glob**, or its exact id -- never anything else.
**Renaming a node stops a name-based silence from matching it.** Match on
the node's id or a tag instead if the silence needs to survive a rename. See
[Fleet alerting: Silences and maintenance
windows](06-alerting-and-channels.md#silences-and-maintenance-windows) for
the full detail.

## Aggregate rules

Aggregate rules have no single-host equivalent: they run only on the master,
evaluated every 30s across a whole tag or the whole fleet.

```
count(tag:web, cpu > 90) >= 3 for 5m
avg(tag:db, mem) > 85 for 10m
online(tag:web) < 2 for 2m
absent(tag:backup, 15m)
```

```bash
trinetra fleet alerting apply rules.json   # rules live inside AlertingConfig.rules
trinetra fleet rules                       # name, state, value, since, expr
```

See [Fleet alerting: Aggregate
rules](06-alerting-and-channels.md#aggregate-rules) for the full grammar,
including the known `storage.backend=memory` limitation for disk-metric
rules against the master's own node.


![Aggregate rules editor](../assets/screenshots/alerting-rules.webp)

## Managed config

The master can push a small, closed set of config keys down to children by
tag -- thresholds, baseline tuning, quiet hours -- so a fleet-wide tuning
change doesn't mean editing every host by hand:

```bash
trinetra fleet managed set --tag web thresholds.cpu_pct=90    # merges into web's fragment
trinetra fleet managed list
trinetra fleet managed status                                 # per-node applied/desired/drift
trinetra fleet managed delete <id>
```

`/fleet/managed` shows the same fragments and a per-node drift table, plus a
create/edit form for admins. A managed key is read-only everywhere else on
that child (CLI, web `/config`, `trinetra-ctl`) until it's unset from the
master's fragments.

![Managed config](../assets/screenshots/managed-config.webp)

See [Fleet alerting: Managed
config](06-alerting-and-channels.md#managed-config) for the exact 10-key
allowlist and the merge-vs-replace semantics of `fleet managed set`.

## Admin: nodes, tokens, and link health

`/fleet/admin` is the one-stop admin page: issuing join tokens, renaming/
retagging/revoking/removing nodes, and a link-health table (skew, outbox
depth, oldest unacked age, replica drop counts) per node. Destructive node
actions require a two-step confirm.

![Fleet admin](../assets/screenshots/fleet-admin.webp)

Tokens and node rename/tag/revoke/remove all have CLI equivalents -- see
[Command reference: trinetra fleet](11-command-reference.md#trinetra-fleet)
-- which is the better fit for scripting a fleet's node inventory. The
link-health columns (outbox bytes, oldest unacked age, gap count) are web-only
today; `trinetra fleet status` and `fleet nodes` surface clock skew and
cumulative replica-drop counts, but not per-node outbox depth.

## Audit log

`/fleet/audit`, admin-only, is a filterable, paginated record of every
fleet mutation: node rename/tag/revoke/remove, token issuance,
silence/maintenance create, alerting config saves, managed-config changes,
and incident acks, each with actor, action, target, and time. There is no
CLI equivalent today; the web page is the only way to read it.

![Audit log](../assets/screenshots/audit-log.webp)

## Revoke, leave, remove, and disable

Four different commands end a node's or a fleet's life, and they are not
interchangeable -- picking the wrong one either leaves a paging node behind
or throws away history you meant to keep.

| I want to... | Run | Effect |
|---|---|---|
| Cut a child off immediately (compromised key, decommissioning now) | `fleet node revoke <node>` on the **master** | Its certificate is refused from now on; it stops shipping and raises its own local "revoked" alert; its replicated history on the master is kept. |
| Also stop tracking it and drop the open node-down alert | `fleet node remove <node>` on the **master** | Everything `revoke` does, plus the node is deleted from the registry and liveness tracking. Its replicated history stays on disk under `fleet/nodes/<id>/`. |
| Turn a child back into a solo host | `fleet leave [--purge]` on the **child** | Local history is always kept. This is local only -- the master is not told, and keeps expecting the node (and paging for it) until you `revoke` or `remove` it there; `leave` prints the exact command. `--purge` also deletes the node's fleet identity and unsent outbox. |
| Turn the master back into a solo host | `fleet disable [--purge]` on the **master** | Without `--purge`, the CA/registry/replicas are kept, so `fleet init` again reuses the same CA and children never have to re-join -- this is also how you re-issue the master's certificate before it expires (see [Operations and troubleshooting](#operations-and-troubleshooting) below). `--purge` deletes all of it. |

A child that has been `leave`-d but not yet `revoke`-d/`remove`-d on the
master will keep showing up as `down` there and keep paging: the two ends
are told about opposite things by design, so nothing is silently orphaned on
either side.

## Operations and troubleshooting

**"Why didn't this alert page me?"** Start with `trinetra fleet explain
<key|id>` -- it prints the exact pipeline trail (silence match, dependency
fold, group, route, policy, delivery, receipt) an alert took, which answers
the question directly far more often than reading config back. `fleet route
test` answers the same question hypothetically, before an alert ever fires.

**Node states, at a glance** (`trinetra fleet nodes`, or the health strip on
`/fleet`):

| State | Meaning |
|---|---|
| `online` | In contact within the last 30s. |
| `lagging` | In contact, but its oldest unsent data is over 5 minutes old, or its clock is more than 30s off. |
| `stale` | No contact for 30s up to `fleet.node_down_after`. |
| `down` | No contact past `fleet.node_down_after` (default 2m). Raises a node-down alert; recovers it on return. |
| `revoked` | Explicitly cut off; never alerts as down. |

If half or more of the fleet (at least three nodes) drops at once, the
master raises one "fleet connectivity" alert instead of paging once per
node -- that is almost always the master's own network, not a mass outage.

**Clock skew.** The master warns once a node's skew passes 30 seconds and
marks it `lagging`; fix NTP on that host. A node whose clock runs *ahead*
will have its points rejected as out-of-order once its series' last stored
timestamp is in the future, so a skewed node can silently stop backfilling
until its clock (and the skew) is corrected.

**Outbox backlog and gaps.** `fleet status` on a child shows outbox size,
unsent record count, and gap count; a long outage grows the outbox until it
hits `fleet.outbox_max_mb` (default 512), at which point the oldest segment
is dropped and recorded as a gap rather than silently lost -- the shipper
repairs gaps, oldest first, from the child's own local history once the link
is back. `fleet status` on the master shows a `replica drops` line and a
clock-skew warning for any node that needs attention.

**Certificate expiry.** The master's server certificate is 2 years and does
not renew itself; from 90 days out the master logs a warning at every start.
Re-issue it with `fleet disable` followed by `fleet init --address ...`
(no `--purge`, so the CA/registry/replicas are kept and children need not
re-join) and a restart. The same sequence changes the addresses the
certificate covers.

**A restored/rolled-back child re-joins as a stranger.** If a child loses or
rolls back its outbox (restoring from backup, or someone deleting
`outbox/`) after its sequence numbers have already passed what the master
applied, the master can silently skip the new records instead of detecting
the divergence. After restoring a child from backup, the safe move is
`fleet leave --purge` on the child, then `fleet join` with a fresh code; the
child rejoins as a new node with a clean sequence, and the old node
(`revoke`/`remove` it on the master, as above) keeps its history under its
old id.

**TLS in front of the fleet port.** Don't put a TLS-terminating reverse
proxy or load balancer in front of `fleet.listen` (default `:9443`): the
master must see the child's client certificate itself to authenticate it,
and a proxy would both strip that and present its own certificate (which
children would refuse against the CA pin). Forward the port at the TCP
level only if anything needs to sit in between.

**Aggregate rules and `storage.backend=memory`.** A master running the
in-memory storage backend excludes its own node from `disk`-metric
aggregate rules, because that backend keeps no queryable 1-minute series to
aggregate over. Use the default `tsfile` backend if a rule needs to see the
master's own disk.

---

[Previous: Roadmap and status](12-roadmap-and-status.md) | [Handbook index](README.md) | [Next: Security](14-security.md)
