import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Login } from './Login';
import type { ApiClient } from '@/lib/apiClient';

describe('Login', () => {
  it('renders the wordmark and a password field', () => {
    const client = { login: vi.fn() } as unknown as ApiClient;
    render(<Login client={client} onSuccess={vi.fn()} />);

    expect(screen.getByText('Sidecars')).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /sign in/i })).toBeInTheDocument();
  });
});
