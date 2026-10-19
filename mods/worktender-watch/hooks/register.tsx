import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { Approval, Signal, Summary } from '../types'
import { capped, describe, fresh, isWorker, parse, parseApprovalOutput, plain, signals, statusLine, summarise, wakePrompt } from './fleet'

const seen = atom({ plugin: 'worktender-watch', key: 'seen' } as const, null as string[] | null)
const dismissed = atom({ plugin: 'worktender-watch', key: 'dismissed' } as const, [] as string[])
const attention = atom({ plugin: 'worktender-watch', key: 'attention' } as const, [] as Signal[])
const summary = atom({ plugin: 'worktender-watch', key: 'summary' } as const, null as Summary | null)

const approval = atom({ plugin: 'worktender-watch', key: 'approval' } as const, null as Approval | null)

const BAND_LINES = 3
const APPROVE_PANE = 'approve'

// worker is this session's answer to "am I a worker?", kept once known: a
// pane does not move between worktrees, and the interval asks on every prompt.
type Config = { bin: string; wake: 'toast' | 'turn'; all: boolean; prFeedback: boolean; prFeedbackRounds: number; refocus: boolean; cwd?: string; worker?: boolean }

// worktender's own default for --max-rounds. The flag is passed only when the
// setting differs, so a mod newer than the binary it runs keeps working at the
// default rather than failing every pass on a flag the binary does not know.
const ROUNDS_DEFAULT = 2

type Look = { counts: Summary; now: Signal[] } | 'worker' | null

// Module scope: a reload starts it over, which is the same as no look in flight.
let polling = false

// One look at the fleet. Answers what it saw for `/wt`; 'worker' when this
// session is one of the workers; null when there is no fleet to see (no
// worktender, herdr unreachable, or a look in flight).
async function poll($: EngineInterface, cfg: Config): Promise<Look> {
  if (polling) return null
  polling = true
  try {
    const argv = [cfg.bin, 'ls', ...(cfg.all ? ['--all-repos'] : []), '--reports', '--json']
    const run = await $.process.run(argv, { cwd: cfg.cwd, timeoutMs: 20_000 })
    // Exit 2 is herdr unreachable; no worktender is a failure to start.
    // Either way there is no fleet to show, and nothing to say about it.
    const doc = run.exitCode === 0 ? parse(run.stdout) : null
    if (!doc) {
      $.ui.status(undefined)
      return null
    }

    const self = await $.env.get('HERDR_PANE_ID')
    if (isWorker(doc, self)) {
      $.ui.status(undefined)
      await update($, attention, () => [])
      return 'worker'
    }
    const now = signals(doc, self)
    const counts = summarise(doc, self)
    await update($, summary, () => counts)
    $.ui.status(statusLine(counts))

    const announced = await read($, seen)
    // The first look in a session is a baseline: whatever was already
    // reported before it started is not news, so it is not announced.
    const news = announced === null ? [] : fresh(now, announced)
    await update($, seen, () => [...new Set([...(announced ?? []), ...now.map(s => s.key)])])

    const gone = new Set(await read($, dismissed))
    await update($, attention, () => now.filter(s => !gone.has(s.key)))

    if (news.length > 0) {
      $.ui.toast(news.length === 1 ? `worktender: ${describe(news[0])}` : `worktender: ${news.length} workers need a look`)
      if (cfg.wake === 'turn') await $.prompt.submit({ text: wakePrompt(news) })
    }

    // Opted in: one pass routing each open PR's failing checks and new
    // reviews to the idle worker on its branch. worktender keeps what it has
    // already said, so this is safe to run every poll. It is one gh call per
    // branch, so it runs last and its failure costs only itself. It covers
    // the session's repository, whatever `scope` says.
    //
    // A PR past its feedback cap is held back from its worker and printed as a
    // CAPPED line; that is the human's to look at, so it becomes a toast.
    // worktender prints it once per piece of held news, so it is not
    // de-duplicated here. It is read even from a pass that exited non-zero:
    // one branch gh could not answer for does not unsay another's.
    if (cfg.prFeedback) {
      const rounds = cfg.prFeedbackRounds === ROUNDS_DEFAULT ? [] : ['--max-rounds', String(cfg.prFeedbackRounds)]
      const fb = await $.process.run([cfg.bin, 'pr-feedback', ...rounds], { cwd: cfg.cwd, timeoutMs: 120_000 })
      if (fb.exitCode !== 0) $.ui.log(`worktender-watch: pr-feedback exited ${fb.exitCode}`, { to: 'debug' })
      const held = capped(fb.stdout)
      if (held.length === 1) {
        $.ui.toast(`worktender: ${held[0].branch} PR #${held[0].pr} hit its feedback cap (${held[0].rounds} rounds) — needs you`)
      } else if (held.length > 1) {
        $.ui.toast(`worktender: ${held.length} PRs hit their feedback cap — need you`)
      }
    }
    return { counts, now }
  } catch (err) {
    $.ui.log(`worktender-watch: ${String(err)}`, { to: 'debug' })
    return null
  } finally {
    polling = false
  }
}

// The goal chain for the worker standing in cwd, as `worktender refocus`
// prints it, or null: not a worker, nothing to refocus on, or no answer.
//
// Worker first, by the same test that keeps the watch quiet in one (its herdr
// pane in a linked worktree), so a coordinator never pays for the gh calls and
// never has a chain for someone else's issue put in its context. refocus is
// silent outside an issue worktree by itself; this is the other half.
//
// ifDue is the interval: `refocus --if-due`, which prints only when the
// worker's role says a delivery is due, and counts the prompt either way.
async function refocus($: EngineInterface, cfg: Config, cwd: string | undefined, ifDue = false): Promise<string | null> {
  if (!(await worker($, cfg, cwd))) return null
  const run = await $.process.run([cfg.bin, 'refocus', ...(ifDue ? ['--if-due'] : [])], { cwd, timeoutMs: 30_000 })
  if (run.exitCode !== 0) {
    $.ui.log(`worktender-watch: refocus exited ${run.exitCode}`, { to: 'debug' })
    return null
  }
  const text = run.stdout.trim()
  return text === '' ? null : text
}

// Whether this session is a worker, asked of `worktender ls` once and then
// remembered. Only an answer is remembered: a look that failed (herdr not up
// yet) is asked again next time rather than taken for "not a worker".
async function worker($: EngineInterface, cfg: Config, cwd: string | undefined): Promise<boolean> {
  if (cfg.worker !== undefined) return cfg.worker
  const self = await $.env.get('HERDR_PANE_ID')
  if (!self) return false
  const ls = await $.process.run([cfg.bin, 'ls', '--json'], { cwd, timeoutMs: 20_000 })
  const doc = ls.exitCode === 0 ? parse(ls.stdout) : null
  if (!doc) return false
  cfg.worker = isWorker(doc, self)
  return cfg.worker
}

// The cap as a whole number of rounds, 0 or more; anything else is the default.
function rounds(v: unknown): number {
  if (v === undefined || v === null || v === '') return ROUNDS_DEFAULT
  const n = Number(v)
  return Number.isInteger(n) && n >= 0 ? n : ROUNDS_DEFAULT
}

export const register: Register = (on, options) => {
  const cfg: Config = {
    bin: String(options.worktender || 'worktender'),
    wake: options.wake === 'turn' ? 'turn' : 'toast',
    all: options.scope === 'all',
    prFeedback: options.prFeedback === true || options.prFeedback === 'true',
    prFeedbackRounds: rounds(options.prFeedbackRounds),
    refocus: options.refocus !== false && options.refocus !== 'false',
  }
  const everyMs = Math.max(5, Number(options.intervalSeconds) || 30) * 1000

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if (!e.isInteractive) return started
    cfg.cwd = e.cwd

    await $.command.register({ name: 'wt', description: 'Look at the worktender fleet now; `/wt undismiss` brings dismissed items back.' })
    void poll($, cfg)
    $.clock.every(everyMs, () => void poll($, cfg))
    return started
  })

  // Refocus: once Claude Code has compacted a worker's conversation, its brief
  // is a summary's worth of what it was, and the worker drifts toward whatever
  // is in front of it. SessionStart with source "compact" is the hook that
  // fires after compaction and can hand the model context (PreCompact cannot:
  // its result carries no additionalContext), so the chain arrives with the
  // first turn after it. worktender reads the chain live from GitHub.
  //
  // On by default, unlike wake=turn and prFeedback: it starts no turn, types
  // into no pane, and runs only in a worker and only after a compaction, so
  // its cost is two gh calls and a few lines of context, rarely. What it puts
  // in context is issue text the worker was already told to read, which
  // refocus quotes bounded and framed as untrusted.
  on('classic.SessionStart', async ($, e, next) => {
    const started = await next(e)
    if (e.source !== 'compact' || !cfg.refocus) return started
    try {
      const chain = await refocus($, cfg, e.cwd)
      if (!chain) return started
      return { ...started, additionalContext: [...(started.additionalContext ?? []), chain] }
    } catch (err) {
      $.ui.log(`worktender-watch: refocus: ${String(err)}`, { to: 'debug' })
      return started
    }
  })

  // Refocus on an interval (#214): the chain again every N prompts or minutes,
  // as the worker's role sets it, for the drift that sets in long before a
  // compaction. worktender keeps the count and decides, because the interval
  // is a role's and roles are its configuration; this hook only asks, on every
  // prompt a worker is sent, and attaches whatever comes back.
  //
  // prompt.submit's context is read by the model beside the prompt and never
  // shown, and attaching it starts no turn: the prompt was starting one
  // anyway. It is attached on the way down, before next, because context put
  // on the result after the prompt entered is not attached. A slash command is
  // not a prompt to the worker and is neither counted nor carries the chain.
  //
  // Off unless a role sets an interval, and that is worktender's to say: with
  // none, `refocus --if-due` reads its configuration and prints nothing,
  // without a gh call. The same `refocus` setting turns this half off too.
  on('prompt.submit', async ($, e, next) => {
    if (!cfg.refocus || e.command || !cfg.cwd) return next(e)
    try {
      const chain = await refocus($, cfg, cfg.cwd, true)
      if (chain) return next({ ...e, context: [...(e.context ?? []), chain] })
    } catch (err) {
      $.ui.log(`worktender-watch: refocus --if-due: ${String(err)}`, { to: 'debug' })
    }
    return next(e)
  })

  on('command.run', { command: 'wt' }, async ($, e) => {
    const [sub, issue] = e.args.trim().split(/\s+/)
    if (sub === 'approve') {
      if (!/^\d+$/.test(issue ?? '')) return { text: 'usage: /wt approve <issue number>' }
      // No stdin: approve prints the payload and its hash, then reads an empty
      // answer and declines. Its exit code is 1 by design; the output is the point.
      const run = await $.process.run([cfg.bin, 'approve', issue, '--repo', cfg.cwd ?? '.'], { cwd: cfg.cwd, timeoutMs: 30_000 })
      const output = run.stdout || run.stderr
      // Close, then replace the state, then open: an open pane redraws on the
      // state write, and would put the old title over the new body.
      await $.ui.close({ id: APPROVE_PANE })
      await update($, approval, () => ({ issue, output: plain(output), shown: parseApprovalOutput(run.stdout), pending: false, result: null }))
      await $.ui.open({ id: APPROVE_PANE, title: `Approve #${issue}` })
      return { text: `Read the payload for #${issue} in the pane, then Approve or Cancel.` }
    }
    if (e.args.trim() === 'undismiss') await update($, dismissed, () => [])
    const look = await poll($, cfg)
    if (look === 'worker') return { text: 'This session is a worker (its pane is in a linked worktree), so the watch stays quiet here; it is for the coordinator.' }
    if (!look) return { text: `No fleet: \`${cfg.bin} ls${cfg.all ? ' --all-repos' : ''} --reports --json\` gave nothing (is herdr running? is this a git repository?), or a look was already in flight.` }
    const lines = look.now.map(s => `- ${describe(s)}`)
    return { text: [statusLine(look.counts) ?? 'wt: nothing working, blocked or to review', ...lines].join('\n') }
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const items = await read($, attention)
    if (e.props.hasSurvey || items.length === 0) return next(e)

    const { Box, Button, Text } = $.ui.resolve(e)
    const shown = items.slice(0, BAND_LINES)
    const more = items.length - shown.length

    return (
      <Box flexDirection="column">
        {shown.map(s => (
          <Text key={s.key} color={s.kind === 'done' ? 'green' : 'yellow'}>
            {s.kind === 'done' ? '✓' : '!'} {describe(s)}
          </Text>
        ))}
        {more > 0 ? <Text dimColor>+{more} more</Text> : null}
        <Box>
          <Button
            key="ask"
            label="Ask Claude to check"
            onPress={async () => {
              await $.prompt.submit({ text: wakePrompt(items) })
            }}
          />
          <Text> </Text>
          <Button
            key="dismiss"
            label="Dismiss"
            onPress={async () => {
              await update($, dismissed, d => [...new Set([...d, ...items.map(s => s.key)])])
              await update($, attention, () => [])
            }}
          />
        </Box>
      </Box>
    )
  })

  // Approvals with an Approve run in flight, by issue and hash: a different
  // approval is never blocked by this one.
  const approving = new Set<string>()

  // The payload goes in a Text exactly as worktender printed it, never as
  // markdown. Approve passes back the hash parsed from this same output, so
  // what is approved is what is on screen; if the issue changed since, worktender
  // refuses (exit 3). Without a parsed hash there is nothing to approve.
  on('ui.render', { component: 'Pane', requestId: APPROVE_PANE }, async ($, e) => {
    const { Box, Button, Text } = $.ui.resolve(e)
    const a = await read($, approval)
    if (!a) return <Text dimColor>Nothing to approve.</Text>

    const close = async () => {
      await update($, approval, () => null)
      await $.ui.close({ id: APPROVE_PANE })
    }

    return (
      <Box flexDirection="column">
        <Text>{a.shown ? `${a.shown.payload}sha256: ${a.shown.sha256}` : a.output}</Text>
        {a.shown ? null : <Text color="red">No sha256 line in that output, so there is nothing to approve.</Text>}
        {a.result ? <Text bold>{a.result}</Text> : null}
        <Box>
          {a.shown && !a.result && !a.pending ? (
            <>
              <Button
                key="approve"
                label="Approve"
                onPress={async () => {
                  const issue = a.issue
                  const sha256 = a.shown!.sha256
                  // Checked and set before any await: update() is not atomic across
                  // two presses, and a second run would post a second approval.
                  const key = `${issue}:${sha256}`
                  if (approving.has(key)) {
                    $.ui.toast(`already approving #${issue}`)
                    return
                  }
                  approving.add(key)
                  const same = (cur: Approval | null) => cur !== null && cur.issue === issue && cur.shown?.sha256 === sha256
                  let result: string
                  try {
                    await update($, approval, cur => (same(cur) ? { ...cur!, pending: true } : cur))
                    const run = await $.process.run([cfg.bin, 'approve', issue, '--repo', cfg.cwd ?? '.', '--sha256', sha256], { cwd: cfg.cwd, timeoutMs: 30_000 })
                    const lines = plain((run.exitCode === 0 ? run.stdout : run.stderr || run.stdout).trim()).split('\n')
                    result = lines[lines.length - 1]
                  } catch (err) {
                    result = `could not run approve: ${plain(String(err))}`
                  } finally {
                    approving.delete(key)
                  }
                  // Only the approval that was clicked gets the result: a newer
                  // `/wt approve` may have replaced it while this ran.
                  await update($, approval, cur => (same(cur) ? { ...cur!, pending: false, result } : cur))
                }}
              />
              <Text> </Text>
            </>
          ) : null}
          <Button key="cancel" label={a.result ? 'Close' : 'Cancel'} onPress={close} />
        </Box>
      </Box>
    )
  })
}
