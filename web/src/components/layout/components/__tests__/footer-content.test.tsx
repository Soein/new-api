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
import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { HtmlContent } from '@/components/html-content'
import { Footer } from '@/components/layout/components/footer'
import { useSystemConfigStore } from '@/stores/system-config-store'

async function renderWithRouter(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
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

describe('Footer custom HTML and navigation contracts', () => {
  let consoleErrorSpy: ReturnType<typeof vi.spyOn> | undefined

  beforeEach(() => {
    useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
  })

  afterEach(() => {
    consoleErrorSpy?.mockRestore()
    consoleErrorSpy = undefined
    useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
    vi.restoreAllMocks()
    cleanup()
  })

  test('sanitizes custom footer HTML through HtmlContent and preserves project attribution', async () => {
    useSystemConfigStore.getState().setConfig({
      footerHtml:
        '<div class="custom-note"><span>Safe Note</span><a href="https://example.com" class="link">Safe Link</a><script>window.__footer_xss = true</script><img src="invalid-src" onerror="window.__footer_xss = true" /><a href="javascript:alert(1)" id="bad-link">Evil Link</a></div>',
    })

    await renderWithRouter(<Footer />)

    // Verify safe content is rendered
    expect(screen.getByText('Safe Note')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Safe Link' })).toHaveAttribute(
      'href',
      'https://example.com'
    )

    // Verify script element is stripped by DOMPurify
    expect(document.querySelector('script')).toBeNull()

    // Verify dangerous onerror attribute and javascript: URL are stripped
    const img = document.querySelector('img[src="invalid-src"]')
    if (img) {
      expect(img).not.toHaveAttribute('onerror')
    }
    const badLink = document.querySelector('#bad-link')
    if (badLink) {
      expect(badLink).not.toHaveAttribute('href', 'javascript:alert(1)')
    }

    // Verify project attribution remains intact
    const attributionLink = screen.getByRole('link', { name: 'New API' })
    expect(attributionLink).toHaveAttribute(
      'href',
      'https://github.com/QuantumNous/new-api'
    )
  })

  test('HtmlContent respects typography={false} by omitting prose classes', () => {
    const { container: containerWithoutProse } = render(
      <HtmlContent content='<p>Plain text</p>' typography={false} />
    )
    const elWithoutProse = containerWithoutProse.firstElementChild
    expect(elWithoutProse?.className).not.toContain('prose')

    const { container: containerWithProse } = render(
      <HtmlContent content='<p>Prose text</p>' typography />
    )
    const elWithProse = containerWithProse.firstElementChild
    expect(elWithProse?.className).toContain('prose')
  })

  test('renders navigation columns with duplicate titles, shared hrefs, and identical links without key collisions', async () => {
    useSystemConfigStore.getState().setConfig({
      footerHtml: '',
      demoSiteEnabled: true,
    })

    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})

    const columns = [
      {
        title: 'Resources',
        links: [
          { text: 'Documentation', href: 'https://example.com/docs' },
          { text: 'API Docs', href: 'https://example.com/docs' },
          { text: 'Documentation', href: 'https://example.com/docs' },
        ],
      },
      {
        title: 'Resources',
        links: [
          { text: 'Guide', href: 'https://example.com/guide' },
          { text: 'Guide', href: 'https://example.com/guide' },
        ],
      },
    ]

    await renderWithRouter(<Footer columns={columns} />)

    const resourceTitles = screen.getAllByText('Resources')
    expect(resourceTitles).toHaveLength(2)

    const docLinks = screen.getAllByRole('link', { name: 'Documentation' })
    expect(docLinks).toHaveLength(2)
    for (const link of docLinks) {
      expect(link).toHaveAttribute('href', 'https://example.com/docs')
    }

    const apiLink = screen.getByRole('link', { name: 'API Docs' })
    expect(apiLink).toHaveAttribute('href', 'https://example.com/docs')

    const guideLinks = screen.getAllByRole('link', { name: 'Guide' })
    expect(guideLinks).toHaveLength(2)
    for (const link of guideLinks) {
      expect(link).toHaveAttribute('href', 'https://example.com/guide')
    }

    expect(consoleErrorSpy).not.toHaveBeenCalled()
  })
})
