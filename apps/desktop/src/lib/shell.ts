/**
 * Thin bridge to the Tauri shell.
 *
 * The same React application runs in two places: inside the desktop window,
 * and in a browser during development. Importing `@tauri-apps/api` directly
 * would break the browser build outright, so every shell capability is reached
 * through this module, which degrades to a no-op when the host is a plain
 * browser. That keeps `npm run dev` usable without a Rust toolchain.
 */

interface TauriInternals {
  invoke?: (cmd: string, args?: Record<string, unknown>) => Promise<unknown>
  event?: {
    listen?: (event: string, handler: (e: { payload: unknown }) => void) => Promise<() => void>
  }
}

function internals(): TauriInternals | null {
  const w = window as unknown as {
    __TAURI_INTERNALS__?: TauriInternals
    __TAURI__?: TauriInternals
  }
  return w.__TAURI_INTERNALS__ ?? w.__TAURI__ ?? null
}

/** Reports whether the app is running inside the desktop shell. */
export function isDesktop(): boolean {
  return internals() !== null
}

/**
 * Calls a command exposed by the Rust shell.
 *
 * Rejects rather than returning undefined when there is no shell: a caller
 * that needs OS access has to handle its absence, and silently resolving would
 * make a missing capability look like a successful no-op.
 */
export async function invoke<T>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const api = internals()
  if (!api?.invoke) {
    throw new Error(`"${cmd}" is only available in the desktop application`)
  }
  return (await api.invoke(cmd, args)) as T
}

/** Calls a shell command, returning null instead of throwing in a browser. */
export async function tryInvoke<T>(
  cmd: string,
  args?: Record<string, unknown>,
): Promise<T | null> {
  try {
    return await invoke<T>(cmd, args)
  } catch {
    return null
  }
}

/**
 * Subscribes to an event emitted by the shell.
 *
 * Returns an unsubscribe function in every case, including when there is no
 * shell, so callers can use it in a `useEffect` cleanup without a null check.
 */
export async function listen<T>(
  event: string,
  handler: (payload: T) => void,
): Promise<() => void> {
  const api = internals()
  if (!api?.event?.listen) return () => {}

  try {
    return await api.event.listen(event, (e) => handler(e.payload as T))
  } catch {
    return () => {}
  }
}
