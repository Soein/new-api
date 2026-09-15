/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRouter,
} from '@tanstack/react-router'
import { act, cleanup, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { NavLinkList } from '@/components/layout/components/nav-link-item'
import { PublicNavigation } from '@/components/layout/components/public-navigation'
import type { TopNavLink } from '@/components/layout/types'
import type { SystemStatus } from '@/features/auth/types'
import { STATUS_QUERY_KEY } from '@/lib/status-query'
import { useAuthStore } from '@/stores/auth-store'

const statusFixture: SystemStatus = {
  HeaderNavModules: JSON.stringify({
    home: false,
    console: false,
    pricing: { enabled: false, requireAuth: false },
    rankings: { enabled: false, requireAuth: false },
    docs: false,
    about: false,
  }),
}

async function renderWithRouter(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(STATUS_QUERY_KEY, statusFixture)
  const rootRoute = createRootRoute({ component: () => ui })
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  return render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

describe('Navigation links key regression and identity retention', () => {
  let consoleErrorSpy: ReturnType<typeof vi.spyOn> | undefined

  beforeEach(() => {
    localStorage.clear()
    useAuthStore.setState(useAuthStore.getInitialState(), true)
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    consoleErrorSpy?.mockRestore()
    consoleErrorSpy = undefined
    localStorage.clear()
    useAuthStore.setState(useAuthStore.getInitialState(), true)
    vi.restoreAllMocks()
    cleanup()
  })

  test('NavLinkList preserves duplicate navigation links without key collision warnings', async () => {
    const links: TopNavLink[] = [
      { title: 'Documentation', href: '/docs' },
      { title: 'Documentation', href: '/docs' },
      { title: 'External', href: 'https://example.com', external: true },
      { title: 'External', href: 'https://example.com', external: true },
    ]

    await renderWithRouter(<NavLinkList links={links} />)

    const docLinks = screen.getAllByRole('link', { name: 'Documentation' })
    expect(docLinks).toHaveLength(2)
    for (const link of docLinks) {
      expect(link).toHaveAttribute('href', '/docs')
    }

    const extLinks = screen.getAllByRole('link', { name: 'External' })
    expect(extLinks).toHaveLength(2)
    for (const link of extLinks) {
      expect(link).toHaveAttribute('href', 'https://example.com')
      expect(link).toHaveAttribute('target', '_blank')
    }

    expect(consoleErrorSpy).not.toHaveBeenCalled()
  })

  test('PublicNavigation preserves duplicate links without key collision warnings', async () => {
    const links: TopNavLink[] = [
      { title: 'Console', href: '/dashboard' },
      { title: 'Console', href: '/dashboard' },
      { title: 'API', href: 'https://api.example.com', external: true },
      { title: 'API', href: 'https://api.example.com', external: true },
    ]

    await renderWithRouter(<PublicNavigation links={links} />)

    const consoleLinks = screen.getAllByRole('link', { name: 'Console' })
    expect(consoleLinks).toHaveLength(2)
    for (const link of consoleLinks) {
      expect(link).toHaveAttribute('href', '/dashboard')
    }

    const apiLinks = screen.getAllByRole('link', { name: 'API' })
    expect(apiLinks).toHaveLength(2)
    for (const link of apiLinks) {
      expect(link).toHaveAttribute('href', 'https://api.example.com')
      expect(link).toHaveAttribute('target', '_blank')
    }

    expect(consoleErrorSpy).not.toHaveBeenCalled()
  })

  test('distinguishes links that would cause delimiter collision in naive title-href concatenation', async () => {
    // Under naive `${title}-${href}`:
    // 'sub-section' + '-' + 'item' => 'sub-section-item'
    // 'sub' + '-' + 'section-item' => 'sub-section-item'
    const collisionLinks: TopNavLink[] = [
      { title: 'sub-section', href: 'item', external: true },
      { title: 'sub', href: 'section-item', external: true },
    ]

    await renderWithRouter(<NavLinkList links={collisionLinks} />)

    const link1 = screen.getByRole('link', { name: 'sub-section' })
    const link2 = screen.getByRole('link', { name: 'sub' })
    expect(link1).toHaveAttribute('href', 'item')
    expect(link2).toHaveAttribute('href', 'section-item')

    expect(consoleErrorSpy).not.toHaveBeenCalled()
  })

  test('NavLinkList preserves user focus and DOM identity across link insertion and reordering', async () => {
    let updateLinks!: (links: TopNavLink[]) => void

    function ListHarness({ initialLinks }: { initialLinks: TopNavLink[] }) {
      const [links, setLinks] = useState(initialLinks)
      updateLinks = setLinks
      return <NavLinkList links={links} />
    }

    const initialLinks: TopNavLink[] = [
      { title: 'First', href: '/first' },
      { title: 'Second', href: '/second' },
      { title: 'Third', href: '/third' },
    ]

    await renderWithRouter(<ListHarness initialLinks={initialLinks} />)

    const secondLink = screen.getByRole('link', { name: 'Second' })
    secondLink.focus()
    expect(document.activeElement).toBe(secondLink)

    // Reorder: Move 'Second' to the front
    act(() => {
      updateLinks([
        { title: 'Second', href: '/second' },
        { title: 'First', href: '/first' },
        { title: 'Third', href: '/third' },
      ])
    })

    const secondLinkAfterReorder = screen.getByRole('link', { name: 'Second' })
    expect(secondLinkAfterReorder).toBe(secondLink)
    expect(document.activeElement).toBe(secondLink)

    // Insert at beginning: Prepend 'Zero'
    act(() => {
      updateLinks([
        { title: 'Zero', href: '/zero' },
        { title: 'Second', href: '/second' },
        { title: 'First', href: '/first' },
        { title: 'Third', href: '/third' },
      ])
    })

    const secondLinkAfterInsert = screen.getByRole('link', { name: 'Second' })
    expect(secondLinkAfterInsert).toBe(secondLink)
    expect(document.activeElement).toBe(secondLink)

    expect(consoleErrorSpy).not.toHaveBeenCalled()
  })

  test('PublicNavigation preserves user focus and DOM identity across reordering', async () => {
    let updateLinks!: (links: TopNavLink[]) => void

    function NavHarness({ initialLinks }: { initialLinks: TopNavLink[] }) {
      const [links, setLinks] = useState(initialLinks)
      updateLinks = setLinks
      return <PublicNavigation links={links} />
    }

    const initialLinks: TopNavLink[] = [
      { title: 'Alpha', href: '/alpha' },
      { title: 'Beta', href: '/beta' },
    ]

    await renderWithRouter(<NavHarness initialLinks={initialLinks} />)

    const betaLink = screen.getByRole('link', { name: 'Beta' })
    betaLink.focus()
    expect(document.activeElement).toBe(betaLink)

    // Reorder
    act(() => {
      updateLinks([
        { title: 'Beta', href: '/beta' },
        { title: 'Alpha', href: '/alpha' },
      ])
    })

    const betaLinkAfter = screen.getByRole('link', { name: 'Beta' })
    expect(betaLinkAfter).toBe(betaLink)
    expect(document.activeElement).toBe(betaLink)

    expect(consoleErrorSpy).not.toHaveBeenCalled()
  })

  test('preserves identity and focus for duplicate links when prepending new items', async () => {
    let updateLinks!: (links: TopNavLink[]) => void

    function ListHarness({ initialLinks }: { initialLinks: TopNavLink[] }) {
      const [links, setLinks] = useState(initialLinks)
      updateLinks = setLinks
      return <NavLinkList links={links} />
    }

    const initialLinks: TopNavLink[] = [
      { title: 'Doc', href: '/doc' },
      { title: 'Doc', href: '/doc' },
    ]

    await renderWithRouter(<ListHarness initialLinks={initialLinks} />)

    const [firstDoc, secondDoc] = screen.getAllByRole('link', { name: 'Doc' })
    secondDoc.focus()
    expect(document.activeElement).toBe(secondDoc)

    // Prepend a new link
    act(() => {
      updateLinks([
        { title: 'Home', href: '/' },
        { title: 'Doc', href: '/doc' },
        { title: 'Doc', href: '/doc' },
      ])
    })

    const docsAfter = screen.getAllByRole('link', { name: 'Doc' })
    expect(docsAfter[0]).toBe(firstDoc)
    expect(docsAfter[1]).toBe(secondDoc)
    expect(document.activeElement).toBe(secondDoc)

    expect(consoleErrorSpy).not.toHaveBeenCalled()
  })
})
