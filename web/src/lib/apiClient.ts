import type {
  BookCandidate,
  BookDetail,
  BookEventsResponse,
  ChangePasswordBody,
  ContributionRow,
  CoreProposal,
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
  SupervisorStatus,
  SupervisorRun,
  BatchCostSummary,
} from '@/api/types';
import { parseContentDispositionFilename } from '@/lib/download';

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

  // authedFetch is the shared request core: it attaches the bearer token (on authed
  // calls), fires onAuthError on a 401, and throws ApiError on any non-2xx. The two
  // public shapes build on it - request() adds JSON body encoding + parsing,
  // exportSidecars reads the raw Blob. Keeping the 401/error handling in one place
  // means both paths get identical dead-token semantics.
  private async authedFetch(
    path: string,
    init: {
      method?: string;
      headers?: Record<string, string>;
      body?: string;
      auth?: boolean;
    } = {},
  ): Promise<Response> {
    const { method = 'GET', body, auth = true } = init;
    const headers: Record<string, string> = { ...init.headers };

    if (auth) {
      const token = this.getToken();
      if (token) {
        headers.Authorization = `Bearer ${token}`;
      }
    }

    const res = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers,
      body,
    });

    if (res.status === 401 && auth) {
      // A dead or missing token on an authed call: clear it and bounce to login.
      this.onAuthError();
    }

    if (!res.ok) {
      throw new ApiError(res.status, await extractError(res));
    }

    return res;
  }

  private async request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
    const { method = 'GET', body, auth = true } = opts;
    const headers: Record<string, string> = {};

    let payload: string | undefined;
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      payload = JSON.stringify(body);
    }

    const res = await this.authedFetch(path, { method, headers, body: payload, auth });

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

  restartDaemon(): Promise<{ restarting: boolean }> {
    return this.request<{ restarting: boolean }>('/api/v1/system/restart', { method: 'POST' });
  }

  getSettings(): Promise<Settings> {
    return this.request<Settings>('/api/v1/settings');
  }

  supervisorStatus(): Promise<SupervisorStatus> {
    return this.request<SupervisorStatus>('/api/v1/supervisor/status');
  }

  supervisorIncidents(batchId?: string, limit = 20): Promise<{ incidents: SupervisorRun[] }> {
    const params = new URLSearchParams({ limit: String(limit) });
    if (batchId) params.set('batch_id', batchId);
    return this.request<{ incidents: SupervisorRun[] }>(`/api/v1/supervisor/incidents?${params}`);
  }

  supervisorCosts(batchId: string): Promise<BatchCostSummary> {
    return this.request<BatchCostSummary>(
      `/api/v1/supervisor/costs?batch_id=${encodeURIComponent(batchId)}`,
    );
  }

  askSupervisor(id: number): Promise<SupervisorRun> {
    return this.request<SupervisorRun>(`/api/v1/books/${id}/ask-supervisor`, { method: 'POST' });
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
  // clamps daemon-side to 1..500 (default 100 when omitted). beforeId is a keyset
  // cursor: only events older than it are returned, so the log view pages back
  // through the whole history to show/download the full backlog.
  getBookEvents(id: number, limit?: number, beforeId?: number): Promise<BookEventsResponse> {
    const params = new URLSearchParams();
    if (limit !== undefined) params.set('limit', String(limit));
    if (beforeId !== undefined) params.set('before_id', String(beforeId));
    const query = params.toString();
    return this.request<BookEventsResponse>(
      `/api/v1/books/${id}/events${query ? `?${query}` : ''}`,
    );
  }

  // --- M7: contribution ---

  // getCoreProposal fetches the prefill for a book's core (add-work) proposal, as
  // written by the contributing stage when it parked the book core_needed. 404 when
  // the daemon has not written one - the caller starts from an empty form.
  getCoreProposal(id: number): Promise<CoreProposal> {
    return this.request<CoreProposal>(`/api/v1/books/${id}/contrib/core`);
  }

  // submitCoreProposal posts the completed add-work proposal. On success the book
  // flips from core_needed to core_pending (the poller resolves the real slug once
  // the metadata PR merges). 400 names the missing field; 409 = wrong park state.
  submitCoreProposal(id: number, proposal: CoreProposal): Promise<ContributionRow> {
    return this.request<ContributionRow>(`/api/v1/books/${id}/contribute/core`, {
      method: 'POST',
      body: proposal,
    });
  }

  // setBookWork attaches an existing meta work slug to a book (the "the work
  // already exists" escape hatch), re-admitting a book parked core_needed/pending.
  // 400 on a bad/unknown slug.
  setBookWork(id: number, workId: string): Promise<BookDetail> {
    return this.request<BookDetail>(`/api/v1/books/${id}/work`, {
      method: 'POST',
      body: { work_id: workId },
    });
  }

  // exportSidecars fetches the book's sidecars as a zip (repo layout) with an authed
  // request - request() parses JSON, so this reads the raw Blob and the download
  // filename from Content-Disposition itself. 404 (ApiError) when the book has no
  // sidecars; the caller disables the Download control on it.
  async exportSidecars(id: number): Promise<{ blob: Blob; filename: string }> {
    const res = await this.authedFetch(`/api/v1/books/${id}/export`);
    const blob = await res.blob();
    const filename = parseContentDispositionFilename(
      res.headers.get('Content-Disposition'),
      `book-${id}-sidecars.zip`,
    );
    return { blob, filename };
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
