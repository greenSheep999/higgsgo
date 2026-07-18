// Bearer token storage. The admin surface is protected by a single per-
// deploy secret (see internal/api/middleware/auth.go). We keep it in
// localStorage for the session — same threat model as the operator having
// the secret in their shell history.

const KEY = "higgsgo.adminBearer";

const listeners = new Set<() => void>();

export function getBearer(): string | null {
  try {
    return localStorage.getItem(KEY);
  } catch {
    return null;
  }
}

export function setBearer(token: string): void {
  try {
    localStorage.setItem(KEY, token);
  } catch {
    /* private mode / disabled storage — the AuthGate will re-prompt. */
  }
  listeners.forEach((fn) => fn());
}

export function clearBearer(): void {
  try {
    localStorage.removeItem(KEY);
  } catch {
    /* ignore */
  }
  listeners.forEach((fn) => fn());
}

export function subscribeBearer(fn: () => void): () => void {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}
