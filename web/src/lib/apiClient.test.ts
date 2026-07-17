import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { ApiClient, ApiError } from './apiClient';

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('ApiClient', () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    vi.stubGlobal('fetch', fetchMock);
    fetchMock.mockReset();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('prepends the base URL and sends the bearer header', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(200, { version: '1', data_dir: '/d', listen: 'x', tabs: [] }),
    );
    const onAuthError = vi.fn();
    const client = new ApiClient({
      baseUrl: 'https://host:8090',
      getToken: () => 'tok-123',
      onAuthError,
    });

    const result = await client.system();

    expect(result).toEqual({ version: '1', data_dir: '/d', listen: 'x', tabs: [] });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('https://host:8090/api/v1/system');
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer tok-123');
    expect(onAuthError).not.toHaveBeenCalled();
  });

  it('returns the parsed body on success', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { token: 'abc' }));
    const client = new ApiClient({
      baseUrl: '',
      getToken: () => null,
      onAuthError: vi.fn(),
    });

    await expect(client.login('pw')).resolves.toEqual({ token: 'abc' });
  });

  it('invokes onAuthError and throws on a 401 from an authed call', async () => {
    fetchMock.mockResolvedValue(jsonResponse(401, { error: 'unauthorized' }));
    const onAuthError = vi.fn();
    const client = new ApiClient({
      baseUrl: '',
      getToken: () => 'dead-token',
      onAuthError,
    });

    await expect(client.system()).rejects.toBeInstanceOf(ApiError);
    expect(onAuthError).toHaveBeenCalledTimes(1);
  });

  it('does not fire onAuthError for a 401 on the login (unauthed) call', async () => {
    fetchMock.mockResolvedValue(jsonResponse(401, { error: 'bad password' }));
    const onAuthError = vi.fn();
    const client = new ApiClient({
      baseUrl: '',
      getToken: () => null,
      onAuthError,
    });

    await expect(client.login('wrong')).rejects.toMatchObject({
      status: 401,
      message: 'bad password',
    });
    expect(onAuthError).not.toHaveBeenCalled();
  });

  it('resolves void for a 204 response', async () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    const client = new ApiClient({
      baseUrl: '',
      getToken: () => 'tok',
      onAuthError: vi.fn(),
    });

    await expect(client.logout()).resolves.toBeUndefined();
  });

  function pipelineClient() {
    return new ApiClient({ baseUrl: '', getToken: () => 'tok', onAuthError: vi.fn() });
  }

  it('POSTs a scan path and reads the job id', async () => {
    fetchMock.mockResolvedValue(jsonResponse(202, { job_id: 'abc123' }));
    const client = pipelineClient();

    await expect(client.createScan('/lib')).resolves.toEqual({ job_id: 'abc123' });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/scans');
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body as string)).toEqual({ path: '/lib' });
  });

  it('GETs a scan by id (url-encoded)', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { id: 'a/b', status: 'running' }));
    const client = pipelineClient();

    await client.getScan('a/b');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/scans/a%2Fb');
  });

  it('POSTs candidates to /books', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { results: [] }));
    const client = pipelineClient();

    await client.createBooks([
      {
        source_path: '/b1',
        title: 'One',
        authors: [],
        series: '',
        series_pos: '',
        asin: '',
        isbn: '',
      },
    ]);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/books');
    expect(JSON.parse(init.body as string).candidates).toHaveLength(1);
  });

  it('GETs the scan list for reattach', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { scans: [] }));
    const client = pipelineClient();

    await expect(client.listScans()).resolves.toEqual({ scans: [] });
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/scans');
  });

  it('POSTs an override upsert', async () => {
    const client = pipelineClient();
    fetchMock.mockResolvedValue(jsonResponse(200, { override: {}, coverage: null }));
    await client.setOverride({ source_path: '/b', hidden: true, work_id: '' });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/overrides');
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body as string)).toEqual({
      source_path: '/b',
      hidden: true,
      work_id: '',
    });
  });

  it('GETs meta search with an encoded query', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { results: [] }));
    const client = pipelineClient();

    await client.metaSearch('dune & sand');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/meta/search?q=dune%20%26%20sand');
  });

  it('GETs a book sidecars preview', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { work: 'dune', characters: [] }));
    const client = pipelineClient();

    await expect(client.getBookSidecars(7)).resolves.toEqual({ work: 'dune', characters: [] });
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/books/7/sidecars');
  });

  it('GETs a book event log without a limit', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { events: [] }));
    const client = pipelineClient();

    await expect(client.getBookEvents(7)).resolves.toEqual({ events: [] });
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/books/7/events');
  });

  it('GETs a book event log with a limit query', async () => {
    fetchMock.mockResolvedValue(jsonResponse(200, { events: [] }));
    const client = pipelineClient();

    await client.getBookEvents(7, 25);
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/books/7/events?limit=25');
  });

  it('routes book control actions to the right endpoints', async () => {
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    const client = pipelineClient();

    await client.pauseBook(7);
    await client.resumeBook(7);
    await client.retryBook(7);
    await client.cancelBook(7);
    await client.deleteBook(7);

    const calls = fetchMock.mock.calls.map(([url, init]) => `${init.method} ${url}`);
    expect(calls).toEqual([
      'POST /api/v1/books/7/pause',
      'POST /api/v1/books/7/resume',
      'POST /api/v1/books/7/retry',
      'POST /api/v1/books/7/cancel',
      'DELETE /api/v1/books/7',
    ]);
  });
});
