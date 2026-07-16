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
});
