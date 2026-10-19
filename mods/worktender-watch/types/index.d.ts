export type SignalKind = 'done' | 'blocked' | 'needs-you'

export type Signal = {
  key: string
  repo: string
  branch: string
  kind: SignalKind
  pr: number | null
}

export type Approval = {
  issue: string
  /** `worktender approve` output as printed, shown whether or not it parsed. */
  output: string
  /** The payload and hash parsed from `output`; null when it had no hash line. */
  shown: { payload: string; sha256: string } | null
  /** What Approve printed, once pressed. */
  result: string | null
}

export type Summary = {
  working: number
  idle: number
  blocked: number
  done: number
  total: number
}

declare module 'claude-code' {
  interface PluginState {
    'worktender-watch': {
      /** Signal keys already announced, so a reload does not announce them again. */
      seen: string[] | null
      /** Keys the person dismissed from the band. */
      dismissed: string[]
      /** What needs attention now, as of the last poll. */
      attention: Signal[]
      summary: Summary | null
      /** The issue `/wt approve` is showing, while its pane is open. */
      approval: Approval | null
    }
  }
}
