export type SignalKind = 'done' | 'blocked' | 'needs-you'

export type Signal = {
  key: string
  repo: string
  branch: string
  kind: SignalKind
  pr: number | null
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
    }
  }
}
