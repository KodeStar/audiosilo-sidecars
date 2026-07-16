import type {
  ChangePasswordBody,
  LoginResponse,
  Settings,
  SettingsUpdate,
  SystemInfo,
} from '@/api/types';

export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export interface ApiClientOptions {
  // Base URL prepended to every request path (empty string = same-origin).
  baseUrl: string;
  // Supplies the current bearer token, or null when signed out.
  getToken: () => string | null;
  // Invoked whenever an authed request returns 401 (dead/missing token). The app
  // wires this to "clear token + go to login".
  onAuthError: () => void;
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  // Auth endpoints like /auth/login run before a token exists.
  auth?: boolean;
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly getToken: () => string | null;
  private readonly onAuthError: () => void;

  constructor(opts: ApiClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, '');
    this.getToken = opts.getToken;
    this.onAuthError = opts.onAuthError;
  }

  private async request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
    const { method = 'GET', body, auth = true } = opts;
    const headers: Record<string, string> = {};

    if (auth) {
      const token = this.getToken();
      if (token) {
        headers.Authorization = `Bearer ${token}`;
      }
    }

    let payload: string | undefined;
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      payload = JSON.stringify(body);
    }

    const res = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers,
      body: payload,
    });

    if (res.status === 401 && auth) {
      // A dead or missing token on an authed call: clear it and bounce to login.
      this.onAuthError();
    }

    if (!res.ok) {
      throw new ApiError(res.status, await extractError(res));
    }

    if (res.status === 204) {
      return undefined as T;
    }

    const text = await res.text();
    if (text === '') {
      return undefined as T;
    }
    return JSON.parse(text) as T;
  }

  // --- auth ---
  login(password: string): Promise<LoginResponse> {
    return this.request<LoginResponse>('/api/v1/auth/login', {
      method: 'POST',
      body: { password },
      auth: false,
    });
  }

  logout(): Promise<void> {
    return this.request<void>('/api/v1/auth/logout', { method: 'POST' });
  }

  changePassword(body: ChangePasswordBody): Promise<void> {
    return this.request<void>('/api/v1/auth/password', {
      method: 'POST',
      body,
    });
  }

  // --- system + settings ---
  system(): Promise<SystemInfo> {
    return this.request<SystemInfo>('/api/v1/system');
  }

  getSettings(): Promise<Settings> {
    return this.request<Settings>('/api/v1/settings');
  }

  updateSettings(update: SettingsUpdate): Promise<Settings> {
    return this.request<Settings>('/api/v1/settings', {
      method: 'PUT',
      body: update,
    });
  }
}

async function extractError(res: Response): Promise<string> {
  try {
    const text = await res.text();
    if (text) {
      const json: unknown = JSON.parse(text);
      if (
        typeof json === 'object' &&
        json !== null &&
        'error' in json &&
        typeof (json as { error: unknown }).error === 'string'
      ) {
        return (json as { error: string }).error;
      }
    }
  } catch {
    // Fall through to a generic message.
  }
  return `Request failed (${res.status})`;
}
