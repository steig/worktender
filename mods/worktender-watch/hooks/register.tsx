import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { Signal, Summary } from '../types'
import { describe, fresh, parse, signals, statusLine, summarise, wakePrompt } from './fleet'

const seen = atom({ plugin: 'worktender-watch', key: 'seen' } as const, null as string[] | null)
const dismissed = atom({ plugin: 'worktender-watch', key: 'dismissed' } as const, [] as string[])
const attention = atom({ plugin: 'worktender-watch', key: 'attention' } as const, [] as Signal[])
const summary = atom({ plugin: 'worktender-watch', key: 'summary' } as const, null as Summary | null)

const BAND_LINES = 3

type Config = { bin: string; wake: 'toast' | 'turn' }

// Module scope: a reload starts it over, which is the same as no look in flight.
let polling = false

// One look at the fleet. Answers what it saw for `/wt`; null when there is
// no fleet to see (no worktender, herdr unreachable, or a look in flight).
async function poll($: EngineInterface, cfg: Config): Promise<{ counts: Summary; now: Signal[] } | null> {
  if (polling) return null
  polling = true
  try {
    const run = await $.process.run([cfg.bin, 'ls', '--all-repos', '--reports', '--json'], { timeoutMs: 20_000 })
    // Exit 2 is herdr unreachable; no worktender is a failure to start.
    // Either way there is no fleet to show, and nothing to say about it.
    const doc = run.exitCode === 0 ? parse(run.stdout) : null
    if (!doc) {
      $.ui.status(undefined)
      return null
    }

    const self = await $.env.get('HERDR_PANE_ID')
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
    return { counts, now }
  } catch (err) {
    $.ui.log(`worktender-watch: ${String(err)}`, { to: 'debug' })
    return null
  } finally {
    polling = false
  }
}


export const register: Register = (on, options) => {
  const cfg: Config = {
    bin: String(options.worktender || 'worktender'),
    wake: options.wake === 'turn' ? 'turn' : 'toast',
  }
  const everyMs = Math.max(5, Number(options.intervalSeconds) || 30) * 1000

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if (!e.isInteractive) return started

    await $.command.register({ name: 'wt', description: 'Look at the worktender fleet now; `/wt undismiss` brings dismissed items back.' })
    void poll($, cfg)
    $.clock.every(everyMs, () => void poll($, cfg))
    return started
  })

  on('command.run', { command: 'wt' }, async ($, e) => {
    if (e.args.trim() === 'undismiss') await update($, dismissed, () => [])
    const look = await poll($, cfg)
    if (!look) return { text: `No fleet: \`${cfg.bin} ls --all-repos --reports --json\` gave nothing (is herdr running?), or a look was already in flight.` }
    const lines = look.now.map(s => `- ${describe(s)}`)
    return { text: [statusLine(look.counts) ?? 'wt: no agents running', ...lines].join('\n') }
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
}
