// The bearer token lives in sessionStorage so it is scoped to the tab/session
// and cleared when the browser is closed.
const TOKEN_KEY = 'audiosilo-sidecars.token';

export function getToken(): string | null {
  try {
    return sessionStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string): void {
  try {
    sessionStorage.setItem(TOKEN_KEY, token);
  } catch {
    // Ignore storage failures (private mode); the in-memory app state still holds it.
  }
}

export function clearToken(): void {
  try {
    sessionStorage.removeItem(TOKEN_KEY);
  } catch {
    // Ignore.
  }
}
