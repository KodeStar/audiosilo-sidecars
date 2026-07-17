import type {
  BookCandidate,
  BookDetail,
  BookEventsResponse,
  ChangePasswordBody,
  CreateBooksResponse,
  CreateScanResponse,
  ListBooksResponse,
  ListScansResponse,
  LoginResponse,
  MetaSearchResponse,
  ScanJob,
  SetOverrideBody,
  SetOverrideResponse,
  Settings,
  SettingsUpdate,
  SidecarsView,
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

  // --- pipeline: scans ---
  createScan(path: string): Promise<CreateScanResponse> {
    return this.request<CreateScanResponse>('/api/v1/scans', {
      method: 'POST',
      body: { path },
    });
  }

  getScan(id: string): Promise<ScanJob> {
    return this.request<ScanJob>(`/api/v1/scans/${encodeURIComponent(id)}`);
  }

  // listScans returns recent scans newest-first (each without its books), so the
  // Library tab can reattach to the last scan after a page reload.
  listScans(): Promise<ListScansResponse> {
    return this.request<ListScansResponse>('/api/v1/scans');
  }

  // --- pipeline: overrides (hide + manual match) ---
  // The daemon also serves GET /api/v1/overrides (for external callers), but the
  // web UI never lists overrides directly - they ride on the scan payload - so no
  // listOverrides client method exists.
  //
  // setOverride upserts a book's full desired override state (hidden=false +
  // work_id="" clears it). The response carries the recomputed coverage when a
  // work_id was set (matched_by "manual"), else null.
  setOverride(body: SetOverrideBody): Promise<SetOverrideResponse> {
    return this.request<SetOverrideResponse>('/api/v1/overrides', {
      method: 'POST',
      body,
    });
  }

  // --- meta search (manual-match lookup) ---
  metaSearch(q: string): Promise<MetaSearchResponse> {
    return this.request<MetaSearchResponse>(`/api/v1/meta/search?q=${encodeURIComponent(q)}`);
  }

  // --- pipeline: books ---
  createBooks(candidates: BookCandidate[]): Promise<CreateBooksResponse> {
    return this.request<CreateBooksResponse>('/api/v1/books', {
      method: 'POST',
      body: { candidates },
    });
  }

  listBooks(): Promise<ListBooksResponse> {
    return this.request<ListBooksResponse>('/api/v1/books');
  }

  // getBook fetches one book's detail view, including its per-stage run ledger
  // (model/token/cost). The list endpoint omits stage_runs, so the Running tab
  // fetches this lazily when a row is expanded.
  getBook(id: number): Promise<BookDetail> {
    return this.request<BookDetail>(`/api/v1/books/${id}`);
  }

  // getBookSidecars fetches the extracted characters/recaps preview for the Done
  // tab, in the metaserve-API shape the vendored expressive.ts consumes. 404 when
  // the book has no sidecar files.
  getBookSidecars(id: number): Promise<SidecarsView> {
    return this.request<SidecarsView>(`/api/v1/books/${id}/sidecars`);
  }

  // getBookEvents fetches the book's durable event-log rows (newest first). limit
  // clamps daemon-side to 1..500 (default 100 when omitted).
  getBookEvents(id: number, limit?: number): Promise<BookEventsResponse> {
    const query = limit === undefined ? '' : `?limit=${limit}`;
    return this.request<BookEventsResponse>(`/api/v1/books/${id}/events${query}`);
  }

  pauseBook(id: number): Promise<void> {
    return this.bookAction(id, 'pause');
  }

  resumeBook(id: number): Promise<void> {
    return this.bookAction(id, 'resume');
  }

  retryBook(id: number): Promise<void> {
    return this.bookAction(id, 'retry');
  }

  cancelBook(id: number): Promise<void> {
    return this.bookAction(id, 'cancel');
  }

  deleteBook(id: number): Promise<void> {
    return this.request<void>(`/api/v1/books/${id}`, { method: 'DELETE' });
  }

  purgeScratch(id: number): Promise<void> {
    return this.request<void>(`/api/v1/books/${id}/purge-scratch`, { method: 'POST' });
  }

  private bookAction(id: number, action: 'pause' | 'resume' | 'retry' | 'cancel'): Promise<void> {
    return this.request<void>(`/api/v1/books/${id}/${action}`, { method: 'POST' });
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
