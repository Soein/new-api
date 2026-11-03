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
  act,
  cleanup,
  render,
  screen,
  within,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StrictMode, useState } from 'react'
import { toast } from 'sonner'
import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  pricingOptions,
  applyPriceSyncSelections,
  pricingValuesByModel,
} from '@/features/model-pricing/pricing'
import { api } from '@/lib/api'
import {
  DEFAULT_CURRENCY_CONFIG,
  useSystemConfigStore,
} from '@/stores/system-config-store'

import { adjustSyncPrice } from '../models/upstream-price-adjustment'
import { SyncPriceCell } from '../models/upstream-price-cells'
import { UpstreamRatioSync } from '../models/upstream-ratio-sync'
import {
  getSyncPriceLines,
  type PricingSourceSelections,
} from '../models/upstream-ratio-sync-helpers'
import { UpstreamRatioSyncTable } from '../models/upstream-ratio-sync-table'
import type { PricingSyncModels } from '../types'

const expression = 'tier("base", p * 2 + c * 8 + cr * 0)'
const differences = {
  m: {
    model_ratio: {
      current: 0.5,
      upstreams: { upstream: 1 },
      confidence: { upstream: true },
    },
    completion_ratio: {
      current: 2,
      upstreams: { upstream: 4 },
      confidence: { upstream: true },
    },
  },
}
const prices: PricingSyncModels = {
  m: {
    current: { model_ratio: 0.5, completion_ratio: 2, billing_mode: 'ratio' },
    upstreams: {
      upstream: {
        model_ratio: 1,
        completion_ratio: 4,
        cache_ratio: 0,
        billing_mode: 'ratio',
      },
    },
  },
}
function TableFixture(props: { prices: PricingSyncModels }) {
  const [selectedSources, setSelectedSources] =
    useState<PricingSourceSelections>({})
  return (
    <UpstreamRatioSyncTable
      prices={props.prices}
      differences={differences}
      selectedSources={selectedSources}
      isDisabled={false}
      isSyncing={false}
      onSelectPrices={(selections) =>
        setSelectedSources((previous) => ({
          ...previous,
          ...Object.fromEntries(
            selections.map((selection) => [selection.model, selection.source])
          ),
        }))
      }
      onUnselectPrices={(models) =>
        setSelectedSources((previous) =>
          Object.fromEntries(
            Object.entries(previous).filter(
              ([model]) => !models.includes(model)
            )
          )
        )
      }
    />
  )
}
afterEach(() => {
  cleanup()
  useSystemConfigStore
    .getState()
    .setConfig({ currency: DEFAULT_CURRENCY_CONFIG })
  vi.restoreAllMocks()
})

describe('pricing synchronization', () => {
  it('shows every dollar price inline, including explicit zero cache and audio prices', () => {
    render(<TableFixture prices={prices} />)
    expect(screen.queryAllByText('Model ratio')).toHaveLength(0)
    expect(screen.getAllByText('$2')).toHaveLength(2)
    expect(screen.getByText('$8')).toBeVisible()
    expect(screen.getByText('$0')).toBeVisible()
    expect(screen.getByText('Cache Read')).toBeVisible()
    expect(
      screen.queryByRole('button', { name: /details/i })
    ).not.toBeInTheDocument()
    expect(
      getSyncPriceLines(
        {
          model_ratio: 2,
          audio_ratio: 3,
          audio_completion_ratio: 4,
          image_ratio: 0,
        },
        (key) => key
      )
    ).toEqual([
      { label: 'Input', value: '$4' },
      { label: 'Image input', value: '$0' },
      { label: 'Audio input', value: '$12' },
      { label: 'Audio output', value: '$48' },
    ])
  })

  it('parses expression prices inline while copying the unchanged source expression', async () => {
    render(
      <TableFixture
        prices={{
          m: {
            current: {},
            upstreams: {
              upstream: {
                billing_mode: 'tiered_expr',
                billing_expr: expression,
              },
            },
          },
        }}
      />
    )
    expect(screen.queryByText(expression)).not.toBeInTheDocument()
    expect(screen.getByText('$2')).toBeVisible()
    expect(screen.getByText('$8')).toBeVisible()
    expect(screen.getByText('$0')).toBeVisible()
    expect(
      screen.getByRole('button', { name: 'Copy billing expression' })
    ).toBeVisible()
    expect(
      screen.queryByRole('button', { name: /details/i })
    ).not.toBeInTheDocument()
    const user = userEvent.setup()
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    await user.click(
      screen.getByRole('button', { name: 'Copy billing expression' })
    )
    expect(copy).toHaveBeenCalledWith(expression)
  })

  it('shows every parsed tier and falls back to the full expression when pricing cannot be parsed safely', () => {
    const tiered =
      'len <= 128000 ? tier("base", p * 2 + c * 8 + cr * 0.2) : tier("long", p * 4 + c * 12 + cr * 0.4)'
    const custom = 'tier("custom", p * 2 + c * 8) * max(1, param("factor"))'
    render(
      <TableFixture
        prices={{
          tiered: {
            current: {},
            upstreams: {
              upstream: { billing_mode: 'tiered_expr', billing_expr: tiered },
            },
          },
          custom: {
            current: {},
            upstreams: {
              upstream: { billing_mode: 'tiered_expr', billing_expr: custom },
            },
          },
        }}
      />
    )
    expect(screen.queryByText(tiered)).not.toBeInTheDocument()
    expect(screen.getByText(/128,000/)).toBeVisible()
    expect(screen.getByText('$0.2')).toBeVisible()
    expect(screen.getByText('$0.4')).toBeVisible()
    expect(screen.getByText(custom)).toBeVisible()
  })

  it('renders multiple expression tiers with identical formatted conditions without duplicate keys and preserves updated prices', () => {
    const errorSpy = vi.spyOn(console, 'error')
    const expression =
      'p < 99999.9999 ? tier("base", p * 1 + c * 2) : p < 100000.0001 ? tier("base", p * 3 + c * 4) : tier("default", p * 5 + c * 6)'
    const updatedExpression =
      'p < 99999.9999 ? tier("base", p * 10 + c * 20) : p < 100000.0001 ? tier("base", p * 30 + c * 40) : tier("default", p * 50 + c * 60)'

    const { rerender } = render(
      <SyncPriceCell
        values={{ billing_mode: 'tiered_expr', billing_expr: expression }}
      />
    )

    expect(screen.getAllByText('Input tokens < 100,000')).toHaveLength(2)
    expect(screen.getByText('$1')).toBeVisible()
    expect(screen.getByText('$2')).toBeVisible()
    expect(screen.getByText('$3')).toBeVisible()
    expect(screen.getByText('$4')).toBeVisible()
    expect(screen.getByText('$5')).toBeVisible()
    expect(screen.getByText('$6')).toBeVisible()

    rerender(
      <SyncPriceCell
        values={{
          billing_mode: 'tiered_expr',
          billing_expr: updatedExpression,
        }}
      />
    )

    expect(screen.getAllByText('Input tokens < 100,000')).toHaveLength(2)
    expect(screen.getByText('$10')).toBeVisible()
    expect(screen.getByText('$20')).toBeVisible()
    expect(screen.getByText('$30')).toBeVisible()
    expect(screen.getByText('$40')).toBeVisible()
    expect(screen.getByText('$50')).toBeVisible()
    expect(screen.getByText('$60')).toBeVisible()
    expect(screen.queryByText('$1')).not.toBeInTheDocument()

    expect(errorSpy).not.toHaveBeenCalled()
  })

  it('shows a mobile comparison list and preserves source-wide selection', async () => {
    const original = window.matchMedia
    vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
      ...original(query),
      matches: query === '(max-width: 640px)',
    }))
    render(<TableFixture prices={prices} />)
    const user = userEvent.setup()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(screen.getByText('m')).toBeVisible()
    await user.click(
      screen.getByRole('checkbox', { name: 'Select all prices from upstream' })
    )
    const source = screen.getByRole('checkbox', {
      name: 'Select price for m from upstream',
    })
    expect(source).toBeChecked()
    source.focus()
    await user.keyboard(' ')
    expect(source).not.toBeChecked()
    expect(source).toHaveFocus()
  })

  it('selects one complete source per model and keeps filtered-out selections', async () => {
    const candidates: PricingSyncModels = {
      ...prices,
      z: {
        current: { model_ratio: 1, billing_mode: 'ratio' },
        upstreams: { upstream: { model_price: 0, billing_mode: 'ratio' } },
      },
    }
    candidates.m = {
      ...candidates.m,
      upstreams: {
        ...candidates.m.upstreams,
        alternative: { billing_mode: 'tiered_expr', billing_expr: expression },
      },
    }
    render(<TableFixture prices={candidates} />)
    const user = userEvent.setup()
    const first = screen.getByRole('checkbox', {
      name: 'Select price for m from upstream',
    })
    await user.click(first)
    await user.click(
      screen.getByRole('checkbox', {
        name: 'Select price for m from alternative',
      })
    )
    expect(first).not.toBeChecked()
    expect(
      screen.getByRole('checkbox', {
        name: 'Select price for m from alternative',
      })
    ).toBeChecked()
    await user.click(
      screen.getByRole('checkbox', { name: 'Select all prices from upstream' })
    )
    expect(first).toBeChecked()
    expect(
      screen.getByRole('checkbox', { name: 'Select price for z from upstream' })
    ).toBeChecked()
    await user.type(screen.getByRole('textbox', { name: 'Search models' }), 'm')
    await waitFor(() =>
      expect(
        screen.queryByRole('checkbox', {
          name: 'Select price for z from upstream',
        })
      ).not.toBeInTheDocument()
    )
    await user.click(
      screen.getByRole('checkbox', { name: 'Select all prices from upstream' })
    )
    await user.clear(screen.getByRole('textbox', { name: 'Search models' }))
    expect(
      await screen.findByRole('checkbox', {
        name: 'Select price for z from upstream',
      })
    ).toBeChecked()
    expect(
      screen.getByRole('checkbox', { name: 'Select price for m from upstream' })
    ).not.toBeChecked()
  })

  it('replaces the selected pricing bundle without mixing legacy fields into expressions', () => {
    const options = pricingOptions({
      ModelRatio: '{"m":1,"untouched":2}',
      CompletionRatio: '{"m":2}',
      CacheRatio: '{"m":0.5}',
    })
    const applied = applyPriceSyncSelections(options, {
      m: {
        billing_mode: 'tiered_expr',
        billing_expr: expression,
        model_ratio: 9,
        model_price: 1,
      },
    })
    expect(pricingValuesByModel(applied).get('m')).toEqual({
      ModelRatio: 1,
      CompletionRatio: 2,
      'billing_setting.billing_mode': 'tiered_expr',
      'billing_setting.billing_expr': expression,
    })
    expect(pricingValuesByModel(applied).get('untouched')).toEqual({
      ModelRatio: 2,
    })
    const legacy = applyPriceSyncSelections(applied, {
      m: { model_ratio: 1, completion_ratio: 4, cache_ratio: 0 },
    })
    expect(pricingValuesByModel(legacy).get('m')).toEqual({
      'billing_setting.billing_mode': 'ratio',
      ModelRatio: 1,
      CompletionRatio: 4,
      CacheRatio: 0,
    })
  })

  it('previews the expression, rejects a stale save and reloads before applying without residual ratio differences', async () => {
    let version = 'v1'
    const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/ratio_sync/channels') {
        return {
          data: {
            success: true,
            data: [
              {
                id: 7,
                name: 'Upstream',
                base_url: 'https://example.test',
                type: 1,
                status: 1,
              },
            ],
          },
        }
      }
      return {
        data: {
          success: true,
          data: {
            entries: [
              {
                model_name: 'm',
                version,
                configured: { ModelRatio: 1, CompletionRatio: 2 },
                effective: { ModelRatio: 1, CompletionRatio: 2 },
              },
            ],
            empty_version: 'empty',
            options: pricingOptions({
              ModelRatio: '{"m":1,"untouched":2}',
              CompletionRatio: '{"m":2}',
            }),
          },
        },
      }
    })
    let saved = false
    const post = vi.spyOn(api, 'post').mockImplementation(async () => ({
      data: {
        success: true,
        data: {
          differences: saved
            ? {}
            : {
                m: {
                  billing_expr: {
                    current: null,
                    upstreams: { 'Upstream(7)': expression },
                    confidence: { 'Upstream(7)': true },
                  },
                },
              },
          prices: {
            m: {
              current: saved
                ? {
                    billing_mode: 'tiered_expr',
                    billing_expr: expression,
                  }
                : {
                    model_ratio: 1,
                    completion_ratio: 2,
                    billing_mode: 'ratio',
                  },
              upstreams: {
                'Upstream(7)': {
                  billing_mode: 'tiered_expr',
                  billing_expr: expression,
                },
              },
            },
          },
          test_results: [{ name: 'Upstream(7)', status: 'success' }],
        },
      },
    }))
    const patch = vi
      .spyOn(api, 'patch')
      .mockResolvedValueOnce({
        data: {
          success: false,
          message: 'Model pricing changed; reload before saving',
        },
      })
      .mockImplementation(async () => {
        saved = true
        return { data: { success: true } }
      })
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    })
    render(
      <QueryClientProvider client={client}>
        <UpstreamRatioSync />
      </QueryClientProvider>
    )
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: 'Select price sources' })
    )
    await user.click(
      await screen.findByRole('checkbox', { name: 'Select Upstream' })
    )
    await user.click(screen.getByRole('button', { name: 'Confirm Selection' }))
    await user.click(
      await screen.findByRole('checkbox', {
        name: 'Select price for m from Upstream(7)',
      })
    )
    expect(screen.getByText('1 selected models')).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Clear selection' }))
    expect(screen.getByRole('button', { name: 'Apply Sync' })).toBeDisabled()
    await user.click(
      screen.getByRole('checkbox', {
        name: 'Select price for m from Upstream(7)',
      })
    )
    await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
    const preview = screen.getByRole('alertdialog', {
      name: 'Preview price changes',
    })
    expect(within(preview).getByText(/Expression pricing/)).toHaveTextContent(
      'Input: $2'
    )
    expect(within(preview).getByText(/Expression pricing/)).toHaveTextContent(
      'Output: $8'
    )
    expect(within(preview).queryByText(expression)).not.toBeInTheDocument()
    expect(patch).not.toHaveBeenCalled()
    await user.click(
      within(preview).getByRole('button', { name: 'Confirm Changes' })
    )
    expect(
      await screen.findByText('Model pricing changed; reload before saving')
    ).toBeVisible()
    expect(
      screen.getByRole('button', { name: 'Confirm Changes' })
    ).toBeDisabled()
    version = 'v2'
    await user.click(screen.getByRole('button', { name: 'Reload pricing' }))
    await user.click(screen.getByRole('button', { name: 'Confirm Selection' }))
    await user.click(
      await screen.findByRole('checkbox', {
        name: 'Select price for m from Upstream(7)',
      })
    )
    await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
    await user.click(screen.getByRole('button', { name: 'Confirm Changes' }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(
      await screen.findByText('No upstream price differences found')
    ).toBeVisible()
    expect(patch).toHaveBeenLastCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'm',
          expected_version: 'v2',
          pricing: {
            ModelRatio: 1,
            CompletionRatio: 2,
            'billing_setting.billing_mode': 'tiered_expr',
            'billing_setting.billing_expr': expression,
          },
        },
      ],
    })
    expect(post).toHaveBeenCalledTimes(3)
    expect(
      get.mock.calls.filter(([url]) => url === '/api/option/model_pricing')
        .length
    ).toBeGreaterThanOrEqual(2)
  })
})

it('shows a source failure instead of a no-differences result and retries the selected source', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/ratio_sync/channels') {
      return {
        data: {
          success: true,
          data: [
            {
              id: 7,
              name: 'Upstream',
              base_url: 'https://example.test',
              type: 1,
              status: 1,
            },
          ],
        },
      }
    }
    return {
      data: {
        success: true,
        data: {
          entries: [],
          empty_version: 'empty',
          options: pricingOptions({}),
        },
      },
    }
  })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: false, message: 'Source unavailable' },
    })
    .mockResolvedValue({
      data: {
        success: true,
        data: {
          prices: {},
          differences: {},
          test_results: [{ name: 'Upstream(7)', status: 'success' }],
        },
      },
    })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <UpstreamRatioSync />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Select price sources' }))
  await user.click(
    await screen.findByRole('checkbox', { name: 'Select Upstream' })
  )
  await user.click(screen.getByRole('button', { name: 'Confirm Selection' }))
  expect(await screen.findByText('Source unavailable')).toBeVisible()
  expect(
    screen.queryByText('No upstream price differences found')
  ).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Apply Sync' })).toBeDisabled()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(
    await screen.findByText('No upstream price differences found')
  ).toBeVisible()
  expect(post).toHaveBeenCalledTimes(2)
  expect(post.mock.calls[0]).toEqual(post.mock.calls[1])
})

it('preserves upstream expression text so a repeated comparison remains unchanged', () => {
  const source =
    'tier("base", p * 2 + c * 8) * (header("service-tier") == "fast" ? 2 : 1)'
  const after = applyPriceSyncSelections(pricingOptions({}), {
    model: { billing_mode: 'tiered_expr', billing_expr: source },
  })
  expect(
    pricingValuesByModel(after).get('model')?.['billing_setting.billing_expr']
  ).toBe(source)
})

describe('sync price adjustments', () => {
  it('keeps source prices by default and scales only the base amount of legacy prices', () => {
    const source = {
      model_ratio: 3.5,
      completion_ratio: 4,
      cache_ratio: 0,
      audio_ratio: 2,
      audio_completion_ratio: 3,
    }
    expect(adjustSyncPrice(source, 'keep', null)).toEqual({ values: source })
    expect(adjustSyncPrice(source, 'divide', 7)).toEqual({
      values: { ...source, model_ratio: 0.5 },
    })
    expect(adjustSyncPrice(source, 'multiply', 7)).toEqual({
      values: { ...source, model_ratio: 24.5 },
    })
    expect(source.model_ratio).toBe(3.5)
    expect(adjustSyncPrice({ model_price: 0 }, 'divide', 7)).toEqual({
      values: { model_price: 0 },
    })
  })

  it('preserves tier thresholds, explicit zero prices, version prefixes and request multipliers', () => {
    const source =
      'v1:(len <= 128000 ? tier("short", p * 7 + c * 21 + cr * 0) : tier("long", fixed(0.7))) * (header("speed") == "fast" ? 3 : 1)'
    expect(
      adjustSyncPrice(
        { billing_mode: 'tiered_expr', billing_expr: source },
        'divide',
        7
      )
    ).toEqual({
      values: {
        billing_mode: 'tiered_expr',
        billing_expr:
          'v1:(len <= 128000 ? tier("short", p * 1 + c * 3 + cr * 0) : tier("long", fixed(0.09999999999999999))) * (header("speed") == "fast" ? 3 : 1)',
      },
    })
  })

  it('scales per_image unit prices in v2 expressions without altering conditions or rule multipliers', () => {
    const source =
      'v2:tier("base", per_image(0.28)) * rule("quality_high", param("quality") == "high", 2)'
    expect(
      adjustSyncPrice(
        { billing_mode: 'tiered_expr', billing_expr: source },
        'divide',
        7
      )
    ).toEqual({
      values: {
        billing_mode: 'tiered_expr',
        billing_expr:
          'v2:tier("base", per_image(0.04)) * rule("quality_high", param("quality") == "high", 2)',
      },
    })
    expect(
      adjustSyncPrice(
        {
          billing_mode: 'tiered_expr',
          billing_expr:
            'v2:tier("base", per_image(0.04)) * rule("quality_high", param("quality") == "high", 2)',
        },
        'multiply',
        7
      )
    ).toEqual({
      values: {
        billing_mode: 'tiered_expr',
        billing_expr: source,
      },
    })
    expect(
      adjustSyncPrice(
        {
          billing_mode: 'tiered_expr',
          billing_expr:
            'v2:tier("base", per_image(0)) * rule("quality_high", param("quality") == "high", 2)',
        },
        'divide',
        7
      )
    ).toEqual({
      values: {
        billing_mode: 'tiered_expr',
        billing_expr:
          'v2:tier("base", per_image(0)) * rule("quality_high", param("quality") == "high", 2)',
      },
    })
    expect(
      adjustSyncPrice(
        {
          billing_mode: 'tiered_expr',
          billing_expr: 'v2:tier("base", p * 7 + per_image(0.28))',
        },
        'divide',
        7
      )
    ).toEqual({
      values: {
        billing_mode: 'tiered_expr',
        billing_expr: 'v2:tier("base", p * 1 + per_image(0.04))',
      },
    })
  })

  it('rejects unsupported expressions, invalid rates and overflow without changing source prices', () => {
    for (const rate of [null, 0, -1, Number.NaN, Infinity]) {
      expect(
        adjustSyncPrice({ model_price: 1 }, 'divide', rate)
      ).toHaveProperty('error')
    }
    for (const expr of [
      'max(p * 7, 10)',
      'tier("x", p * 7) + 5',
      'tier("x", p * 7) * max(1, param("factor"))',
      'v2:tier("base", per_image(0.28) + 5)',
      'v2:tier("base", per_image(-0.28))',
      'v2:tier("base", per_image(0.28) + per_image(0.14))',
      'v2:tier("base", per_image(0.28) * 2)',
      'v1:tier("base", per_image(0.28))',
    ]) {
      expect(
        adjustSyncPrice(
          { billing_mode: 'tiered_expr', billing_expr: expr },
          'divide',
          7
        )
      ).toHaveProperty('error')
    }
    expect(
      adjustSyncPrice({ model_price: Number.MAX_VALUE }, 'multiply', 7)
    ).toHaveProperty('error')
    expect(
      adjustSyncPrice({ model_price: Number.MIN_VALUE }, 'divide', 7)
    ).toHaveProperty('error')
  })
})

async function renderAdjustmentSync(
  candidates: PricingSyncModels = {
    a: {
      current: {},
      upstreams: {
        'Upstream(7)': { model_price: 7 },
        alternative: { model_price: 14 },
      },
    },
    b: {
      current: {},
      upstreams: {
        'Upstream(7)': {
          model_ratio: 3.5,
          completion_ratio: 4,
          cache_ratio: 0,
        },
      },
    },
    c: { current: {}, upstreams: { 'Upstream(7)': { model_price: 21 } } },
  },
  fixtureOptions?: {
    strictMode?: boolean
    queryClient?: QueryClient
    getModelPricing?: (
      requested: string[],
      config?: unknown
    ) => Promise<{ data: unknown }> | { data: unknown }
    getComparisonResponse?: (
      request: unknown
    ) => Promise<{ data: unknown }> | { data: unknown }
  }
) {
  useSystemConfigStore.getState().setConfig({
    currency: {
      ...DEFAULT_CURRENCY_CONFIG,
      quotaDisplayType: 'CNY',
      usdExchangeRate: 7,
    },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url, config) => {
    if (url === '/api/ratio_sync/channels') {
      return {
        data: {
          success: true,
          data: [
            {
              id: 7,
              name: 'Upstream',
              base_url: 'https://example.test',
              type: 1,
              status: 1,
            },
          ],
        },
      }
    }
    const params = config?.params as URLSearchParams | undefined
    const requested = params?.getAll?.('model') ?? []
    if (fixtureOptions?.getModelPricing) {
      return fixtureOptions.getModelPricing(requested, config)
    }
    return {
      data: {
        success: true,
        data: {
          entries: requested.map((name: string) => ({
            model_name: name,
            numeric_model_name: name,
            version: 'empty',
            configured: {},
            effective: {},
          })),
          empty_version: 'empty',
          options: pricingOptions({}),
        },
      },
    }
  })
  const post = vi.spyOn(api, 'post').mockImplementation(async (url, data) => {
    if (
      url === '/api/ratio_sync/fetch' &&
      fixtureOptions?.getComparisonResponse
    ) {
      return fixtureOptions.getComparisonResponse(data)
    }
    return {
      data: {
        success: true,
        data: {
          prices: candidates,
          differences: {},
          test_results: [{ name: 'Upstream(7)', status: 'success' }],
        },
      },
    }
  })
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true } })
  const client =
    fixtureOptions?.queryClient ??
    new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    })
  const ui = (
    <QueryClientProvider client={client}>
      <UpstreamRatioSync />
    </QueryClientProvider>
  )
  render(fixtureOptions?.strictMode ? <StrictMode>{ui}</StrictMode> : ui)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Select price sources' }))
  await user.click(
    await screen.findByRole('checkbox', { name: 'Select Upstream' })
  )
  await user.click(screen.getByRole('button', { name: 'Confirm Selection' }))
  await waitFor(() =>
    expect(
      screen.queryByText('Fetching upstream prices...')
    ).not.toBeInTheDocument()
  )
  return { user, patch, post }
}

it('applies bulk changes only to existing selections, allows row overrides and saves the frozen preview', async () => {
  const { user, patch } = await renderAdjustmentSync()
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for b from Upstream(7)',
    })
  )
  expect(
    screen.getByRole('button', {
      name: 'Price adjustment for a from Upstream(7)',
    })
  ).toHaveAttribute('title', 'Keep source price')
  await user.click(
    screen.getByRole('combobox', { name: 'Set selected prices' })
  )
  await user.click(
    screen.getByRole('option', { name: 'Divide by exchange rate (÷ 7)' })
  )
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for c from Upstream(7)',
    })
  )
  expect(
    screen.getByRole('button', {
      name: 'Price adjustment for c from Upstream(7)',
    })
  ).toHaveAttribute('title', 'Keep source price')
  await user.click(
    screen.getByRole('button', {
      name: 'Price adjustment for b from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('menuitemradio', {
      name: 'Multiply by exchange rate (× 7)',
    })
  )
  await user.type(screen.getByRole('textbox', { name: 'Search models' }), 'a')
  await waitFor(() =>
    expect(
      screen.queryByRole('button', {
        name: 'Price adjustment for b from Upstream(7)',
      })
    ).not.toBeInTheDocument()
  )
  await user.clear(screen.getByRole('textbox', { name: 'Search models' }))
  expect(
    await screen.findByRole('button', {
      name: 'Price adjustment for b from Upstream(7)',
    })
  ).toHaveAttribute('title', 'Multiply by exchange rate (× 7)')
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  const dialog = screen.getByRole('alertdialog', {
    name: 'Preview price changes',
  })
  const rowA = within(dialog).getByText('a').closest('tr')
  expect(rowA).toHaveTextContent('Per-request: $1')
  expect(rowA).toHaveTextContent('Per-request: ¥7')
  expect(within(dialog).getByText('b').closest('tr')).toHaveTextContent(
    'Input: $49'
  )
  expect(within(dialog).getByText('c').closest('tr')).toHaveTextContent(
    'Per-request: $21'
  )
  await act(async () =>
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: 'CNY',
        usdExchangeRate: 8,
      },
    })
  )
  expect(
    within(dialog).getByText('Current exchange rate: 1 USD = 7 CNY')
  ).toBeVisible()
  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )
  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: expect.arrayContaining([
        {
          model_name: 'a',
          expected_version: 'empty',
          pricing: { 'billing_setting.billing_mode': 'ratio', ModelPrice: 1 },
        },
        {
          model_name: 'b',
          expected_version: 'empty',
          pricing: {
            'billing_setting.billing_mode': 'ratio',
            ModelRatio: 24.5,
            CompletionRatio: 4,
            CacheRatio: 0,
          },
        },
        {
          model_name: 'c',
          expected_version: 'empty',
          pricing: { 'billing_setting.billing_mode': 'ratio', ModelPrice: 21 },
        },
      ]),
    })
  )
  expect(patch.mock.calls[0][1]).toHaveProperty('changes.length', 3)
})

it('locks table interactions during slow cache invalidation and reloads authoritatively once', async () => {
  let resolveInvalidate: () => void = () => {}
  const invalidatePromise = new Promise<void>((resolve) => {
    resolveInvalidate = resolve
  })

  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const origInvalidate = client.invalidateQueries.bind(client)
  const invalidateSpy = vi
    .spyOn(client, 'invalidateQueries')
    .mockImplementation(async (filters, options) => {
      await invalidatePromise
      return origInvalidate(filters, options)
    })

  const { user, patch } = await renderAdjustmentSync(undefined, {
    queryClient: client,
  })

  const selectA = await screen.findByRole('checkbox', {
    name: 'Select price for a from Upstream(7)',
  })
  await user.click(selectA)
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))

  const dialog = screen.getByRole('alertdialog', {
    name: 'Preview price changes',
  })
  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )

  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1))
  await waitFor(() => expect(invalidateSpy).toHaveBeenCalled())
  expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()

  // During slow invalidation window, table shows loading state and controls are locked/disabled
  expect(screen.getByText('Fetching upstream prices...')).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Select price sources' })
  ).toBeDisabled()
  expect(screen.getByRole('button', { name: 'Apply Sync' })).toBeDisabled()
  expect(
    screen.getByRole('combobox', { name: 'Set selected prices' })
  ).toBeDisabled()
  expect(
    screen.getByRole('button', { name: /Multiply by exchange rate/ })
  ).toBeDisabled()
  expect(
    screen.getByRole('button', { name: /Divide by exchange rate/ })
  ).toBeDisabled()

  // Release deferred invalidation
  resolveInvalidate()

  // Wait for post-sync refresh to complete
  await waitFor(() =>
    expect(
      screen.queryByText('Fetching upstream prices...')
    ).not.toBeInTheDocument()
  )

  // Controls are now unlocked
  await waitFor(() => {
    expect(
      screen.getByRole('checkbox', {
        name: 'Select price for a from Upstream(7)',
      })
    ).not.toBeDisabled()
  })
  expect(
    screen.getByRole('checkbox', {
      name: 'Select price for b from Upstream(7)',
    })
  ).not.toBeDisabled()
  expect(
    screen.getByRole('button', { name: 'Select price sources' })
  ).not.toBeDisabled()

  // Authoritative reload occurred once, exactly one PATCH was made
  expect(patch).toHaveBeenCalledTimes(1)
})

it('does not repeat a confirmed successful PATCH when baseline reload fails and is retried', async () => {
  let failBaseline = false
  const toastErrorSpy = vi.spyOn(toast, 'error')
  const { user, patch } = await renderAdjustmentSync(undefined, {
    getModelPricing: async (requested) => {
      if (failBaseline && requested.length === 0) {
        throw new Error('Simulated baseline load failure')
      }
      return {
        data: {
          success: true,
          data: {
            entries: requested.map((name: string) => ({
              model_name: name,
              numeric_model_name: name,
              version: 'empty',
              configured: {},
              effective: {},
            })),
            empty_version: 'empty',
            options: pricingOptions({}),
          },
        },
      }
    },
  })

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))

  const dialog = screen.getByRole('alertdialog', {
    name: 'Preview price changes',
  })

  failBaseline = true
  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )

  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1))
  expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()

  await waitFor(() =>
    expect(toastErrorSpy).toHaveBeenCalledWith(
      expect.stringContaining('Simulated baseline load failure')
    )
  )

  expect(patch).toHaveBeenCalledTimes(1)
})

it('does not compound repeated bulk conversion and resets adjustments when switching sources or clearing selection', async () => {
  const { user } = await renderAdjustmentSync()
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  )
  for (let i = 0; i < 2; i++) {
    await user.click(
      screen.getByRole('combobox', { name: 'Set selected prices' })
    )
    await user.click(
      screen.getByRole('option', { name: 'Divide by exchange rate (÷ 7)' })
    )
  }
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  expect(
    within(screen.getByRole('alertdialog')).getByText(/Per-request: \$1/)
  ).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for a from alternative',
    })
  )
  expect(
    screen.getByRole('button', {
      name: 'Price adjustment for a from alternative',
    })
  ).toHaveAttribute('title', 'Keep source price')
  await user.click(
    screen.getByRole('button', {
      name: 'Price adjustment for a from alternative',
    })
  )
  await user.click(
    screen.getByRole('menuitemradio', {
      name: 'Multiply by exchange rate (× 7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Clear selection' }))
  expect(
    screen.getByRole('combobox', { name: 'Set selected prices' })
  ).toBeDisabled()
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for a from alternative',
    })
  )
  expect(
    screen.getByRole('button', {
      name: 'Price adjustment for a from alternative',
    })
  ).toHaveAttribute('title', 'Keep source price')
})

it('allows matching prices to be selected and adjusted in the mobile list', async () => {
  const original = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...original(query),
    matches: query === '(max-width: 640px)',
  }))
  const { user, patch } = await renderAdjustmentSync({
    same: {
      current: { model_price: 7 },
      upstreams: { 'Upstream(7)': { model_price: 7 } },
    },
  })
  expect(
    screen.queryByRole('checkbox', {
      name: 'Select price for same from Upstream(7)',
    })
  ).not.toBeInTheDocument()
  await user.click(
    screen.getByRole('checkbox', { name: 'Show models with matching prices' })
  )
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for same from Upstream(7)',
    })
  )
  const selector = screen.getByRole('button', {
    name: 'Price adjustment for same from Upstream(7)',
  })
  selector.focus()
  await user.keyboard('{Enter}')
  await user.click(
    screen.getByRole('menuitemradio', { name: 'Divide by exchange rate (÷ 7)' })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  expect(screen.getByRole('alertdialog')).toHaveTextContent('Per-request: $1')
  await user.click(screen.getByRole('button', { name: 'Confirm Changes' }))
  await waitFor(() => expect(patch).toHaveBeenCalled())
})

it('blocks unsupported expressions until restored and disables conversion for invalid site rates', async () => {
  const { user } = await renderAdjustmentSync({
    custom: {
      current: {},
      upstreams: {
        'Upstream(7)': {
          billing_mode: 'tiered_expr',
          billing_expr: 'max(p * 7, 10)',
        },
      },
    },
  })
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for custom from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('button', {
      name: 'Price adjustment for custom from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('menuitemradio', { name: 'Divide by exchange rate (÷ 7)' })
  )
  expect(screen.getByRole('alert')).toHaveTextContent(
    'This expression cannot be adjusted automatically.'
  )
  expect(screen.getByRole('button', { name: 'Apply Sync' })).toBeDisabled()
  await user.click(
    screen.getByRole('button', {
      name: 'Price adjustment for custom from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('menuitemradio', { name: 'Keep source price' })
  )
  expect(screen.getByRole('button', { name: 'Apply Sync' })).toBeEnabled()
  await act(async () =>
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: 'CNY',
        usdExchangeRate: 0,
      },
    })
  )
  await user.click(
    screen.getByRole('combobox', { name: 'Set selected prices' })
  )
  expect(
    screen.getByRole('option', { name: /Divide by exchange rate/ })
  ).toHaveAttribute('aria-disabled', 'true')
  expect(
    screen.getByRole('option', { name: 'Keep source price' })
  ).not.toHaveAttribute('aria-disabled', 'true')
})

it('uses the custom currency rate and blocks an invalid rate after a conversion is selected', async () => {
  const { user } = await renderAdjustmentSync()
  await act(async () =>
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: 'CUSTOM',
        customCurrencySymbol: '€',
        customCurrencyExchangeRate: 2,
        usdExchangeRate: 7,
      },
    })
  )
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('button', {
      name: 'Price adjustment for a from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('menuitemradio', { name: 'Divide by exchange rate (÷ 2)' })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  const dialog = screen.getByRole('alertdialog')
  expect(dialog).toHaveTextContent('Per-request: $3.5')
  expect(dialog).toHaveTextContent('Per-request: €7')
  await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  await act(async () =>
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: 'CUSTOM',
        customCurrencySymbol: '€',
        customCurrencyExchangeRate: 0,
      },
    })
  )
  expect(screen.getByRole('alert')).toHaveTextContent(
    'A valid site exchange rate is required.'
  )
  expect(screen.getByRole('button', { name: 'Apply Sync' })).toBeDisabled()
  await user.click(
    screen.getByRole('button', {
      name: 'Price adjustment for a from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('menuitemradio', { name: 'Keep source price' })
  )
  expect(screen.getByRole('button', { name: 'Apply Sync' })).toBeEnabled()
})

it('shows a positive converted price below display precision without labeling it free', () => {
  const adjusted = adjustSyncPrice({ model_price: 1e-10 }, 'divide', 1e7)
  expect(adjusted.values).toBeDefined()
  expect(getSyncPriceLines(adjusted.values ?? {}, (key) => key)).toEqual([
    { label: 'Per-request', value: '$1e-17' },
  ])
})

it('adjusts a price from its source cell without expanding the model column or requiring prior selection', async () => {
  const { user } = await renderAdjustmentSync()
  const button = await screen.findByRole('button', {
    name: 'Price adjustment for a from Upstream(7)',
  })
  const row = button.closest('tr')
  expect(row).not.toBeNull()
  expect(button.closest('td')).toHaveTextContent('$7')
  expect(
    screen.queryByRole('combobox', { name: /Price adjustment for/ })
  ).not.toBeInTheDocument()
  expect(screen.queryByText('Final saved price')).not.toBeInTheDocument()
  await user.click(button)
  expect(
    screen.getByRole('menuitemradio', { name: 'Keep source price' })
  ).toHaveAttribute('aria-checked', 'true')
  await user.keyboard('{Escape}')
  expect(
    screen.getByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  ).not.toBeChecked()
  expect(button).toHaveFocus()
  await user.click(button)
  await user.click(
    screen.getByRole('menuitemradio', { name: 'Divide by exchange rate (÷ 7)' })
  )
  expect(
    screen.getByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  ).toBeChecked()
  expect(button).toHaveTextContent('÷ 7')
  expect(button.closest('td')).toHaveTextContent('$1')
  expect(button.closest('td')).not.toHaveTextContent('$7')
  expect(screen.getByText('a').closest('td')).not.toHaveTextContent('÷ 7')
  expect(screen.queryByText('Final saved price')).not.toBeInTheDocument()
  await user.click(button)
  expect(
    screen.getByRole('menuitemradio', { name: 'Divide by exchange rate (÷ 7)' })
  ).toHaveAttribute('aria-checked', 'true')
  await user.click(
    screen.getByRole('menuitemradio', { name: 'Keep source price' })
  )
  expect(button.closest('td')).toHaveTextContent('$7')
  expect(button).toHaveAttribute('title', 'Keep source price')
})

it('applies multiply and divide shortcuts to selected models across filters without affecting later selections', async () => {
  const { user } = await renderAdjustmentSync()
  const multiply = screen.getByRole('button', {
    name: 'Multiply by exchange rate (× 7)',
  })
  const divide = screen.getByRole('button', {
    name: 'Divide by exchange rate (÷ 7)',
  })
  expect(multiply).toBeDisabled()
  expect(divide).toBeDisabled()
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for b from Upstream(7)',
    })
  )
  await user.type(screen.getByRole('textbox', { name: 'Search models' }), 'a')
  await waitFor(() =>
    expect(
      screen.queryByRole('checkbox', {
        name: 'Select price for b from Upstream(7)',
      })
    ).not.toBeInTheDocument()
  )
  await user.click(multiply)
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  expect(
    screen.getByRole('button', {
      name: 'Price adjustment for a from Upstream(7)',
    })
  ).toHaveAttribute('title', 'Multiply by exchange rate (× 7)')
  await user.click(divide)
  await user.click(divide)
  await user.clear(screen.getByRole('textbox', { name: 'Search models' }))
  expect(
    await screen.findByRole('button', {
      name: 'Price adjustment for b from Upstream(7)',
    })
  ).toHaveAttribute('title', 'Divide by exchange rate (÷ 7)')
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for c from Upstream(7)',
    })
  )
  expect(
    screen.getByRole('button', {
      name: 'Price adjustment for c from Upstream(7)',
    })
  ).toHaveAttribute('title', 'Keep source price')
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  const dialog = screen.getByRole('alertdialog')
  expect(within(dialog).getByText('a').closest('tr')).toHaveTextContent(
    'Per-request: $1'
  )
  expect(within(dialog).getByText('b').closest('tr')).toHaveTextContent(
    'Input: $1'
  )
  expect(within(dialog).getByText('c').closest('tr')).toHaveTextContent(
    'Per-request: $21'
  )
})

it('disables both batch shortcuts when the configured exchange rate becomes invalid', async () => {
  const { user } = await renderAdjustmentSync()
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for a from Upstream(7)',
    })
  )
  await act(async () =>
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: 'CNY',
        usdExchangeRate: 0,
      },
    })
  )
  expect(
    screen.getByRole('button', { name: /Multiply by exchange rate/ })
  ).toBeDisabled()
  expect(
    screen.getByRole('button', { name: /Divide by exchange rate/ })
  ).toBeDisabled()
})

it('syncs v2 image rule expressions alongside flat price models when dividing by exchange rate', async () => {
  const imageExpr =
    'v2:tier("base", per_image(0.28)) * rule("quality_high", param("quality") == "high", 2)'
  const { user, patch } = await renderAdjustmentSync({
    'image-rule': {
      current: {},
      upstreams: {
        'Upstream(7)': {
          billing_mode: 'tiered_expr',
          billing_expr: imageExpr,
        },
      },
    },
    'sync-flat': {
      current: {},
      upstreams: {
        'Upstream(7)': { model_price: 7 },
      },
    },
  })
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for image-rule from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for sync-flat from Upstream(7)',
    })
  )
  await user.click(
    screen.getByRole('combobox', { name: 'Set selected prices' })
  )
  await user.click(
    screen.getByRole('option', { name: 'Divide by exchange rate (÷ 7)' })
  )
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  const applyButton = screen.getByRole('button', { name: 'Apply Sync' })
  expect(applyButton).toBeEnabled()
  await user.click(applyButton)
  const dialog = screen.getByRole('alertdialog', {
    name: 'Preview price changes',
  })
  expect(dialog).toBeVisible()
  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )
  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: expect.arrayContaining([
        {
          model_name: 'image-rule',
          expected_version: 'empty',
          pricing: {
            'billing_setting.billing_mode': 'tiered_expr',
            'billing_setting.billing_expr':
              'v2:tier("base", per_image(0.04)) * rule("quality_high", param("quality") == "high", 2)',
          },
        },
        {
          model_name: 'sync-flat',
          expected_version: 'empty',
          pricing: {
            'billing_setting.billing_mode': 'ratio',
            ModelPrice: 1,
          },
        },
      ]),
    })
  )
})

it('resolves wildcard alias in StrictMode, handles cancel and re-apply, and submits expected version', async () => {
  const { user, patch } = await renderAdjustmentSync(
    {
      'gpt-4o-gizmo-new': {
        current: { model_price: 1 },
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
    },
    {
      strictMode: true,
      getModelPricing: (requested) => ({
        data: {
          success: true,
          data: {
            entries: requested.includes('gpt-4o-gizmo-new')
              ? [
                  {
                    model_name: 'gpt-4o-gizmo-new',
                    numeric_model_name: 'gpt-4o-gizmo-*',
                    version: 'alias-v1',
                    configured: { ModelPrice: 1 },
                    effective: { ModelPrice: 1 },
                  },
                ]
              : [],
            empty_version: 'empty',
            options: pricingOptions({
              ModelPrice: '{"gpt-4o-gizmo-*":1}',
            }),
          },
        },
      }),
    }
  )

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-new from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  const dialog = await screen.findByRole('alertdialog', {
    name: 'Preview price changes',
  })
  expect(within(dialog).getByText('gpt-4o-gizmo-new')).toBeVisible()

  await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(patch).not.toHaveBeenCalled()

  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  const reopened = await screen.findByRole('alertdialog', {
    name: 'Preview price changes',
  })
  await user.click(
    within(reopened).getByRole('button', { name: 'Confirm Changes' })
  )
  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'gpt-4o-gizmo-new',
          expected_version: 'alias-v1',
          pricing: {
            ModelPrice: 2,
            'billing_setting.billing_mode': 'ratio',
          },
        },
      ],
    })
  )
})

it('rejects conflicting selected numeric aliases with no preview and no writes', async () => {
  const { user, patch } = await renderAdjustmentSync(
    {
      'gpt-4o-gizmo-a': {
        current: {},
        upstreams: { 'Upstream(7)': { model_price: 1 } },
      },
      'gpt-4o-gizmo-b': {
        current: {},
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
    },
    {
      getModelPricing: (requested) => ({
        data: {
          success: true,
          data: {
            entries: [
              ...(requested.includes('gpt-4o-gizmo-a')
                ? [
                    {
                      model_name: 'gpt-4o-gizmo-a',
                      numeric_model_name: 'gpt-4o-gizmo-*',
                      version: 'alias-v1',
                      configured: { ModelPrice: 1 },
                      effective: { ModelPrice: 1 },
                    },
                  ]
                : []),
              ...(requested.includes('gpt-4o-gizmo-b')
                ? [
                    {
                      model_name: 'gpt-4o-gizmo-b',
                      numeric_model_name: 'gpt-4o-gizmo-*',
                      version: 'alias-v2',
                      configured: { ModelPrice: 1 },
                      effective: { ModelPrice: 1 },
                    },
                  ]
                : []),
            ],
            empty_version: 'empty',
            options: pricingOptions({
              ModelPrice: '{"gpt-4o-gizmo-*":1}',
            }),
          },
        },
      }),
    }
  )

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-a from Upstream(7)',
    })
  )
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-b from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))

  await waitFor(() => {
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })
  expect(patch).not.toHaveBeenCalled()
})

it('allows compatible mixed expression and numeric alias selections to preview and save', async () => {
  const { user, patch } = await renderAdjustmentSync(
    {
      'gpt-4o-gizmo-a': {
        current: { model_price: 1 },
        upstreams: {
          'Upstream(7)': {
            billing_mode: 'tiered_expr',
            billing_expr: 'tier("base", p * 2)',
          },
        },
      },
      'gpt-4o-gizmo-b': {
        current: { model_price: 1 },
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
    },
    {
      getModelPricing: (requested) => ({
        data: {
          success: true,
          data: {
            entries: [
              ...(requested.includes('gpt-4o-gizmo-a')
                ? [
                    {
                      model_name: 'gpt-4o-gizmo-a',
                      numeric_model_name: 'gpt-4o-gizmo-*',
                      version: 'alias-v1',
                      configured: { ModelPrice: 1 },
                      effective: { ModelPrice: 1 },
                    },
                  ]
                : []),
              ...(requested.includes('gpt-4o-gizmo-b')
                ? [
                    {
                      model_name: 'gpt-4o-gizmo-b',
                      numeric_model_name: 'gpt-4o-gizmo-*',
                      version: 'alias-v2',
                      configured: { ModelPrice: 1 },
                      effective: { ModelPrice: 1 },
                    },
                  ]
                : []),
            ],
            empty_version: 'empty',
            options: pricingOptions({
              ModelPrice: '{"gpt-4o-gizmo-*":1}',
            }),
          },
        },
      }),
    }
  )

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-a from Upstream(7)',
    })
  )
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-b from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))

  const dialog = await screen.findByRole('alertdialog', {
    name: 'Preview price changes',
  })
  expect(within(dialog).getByText('gpt-4o-gizmo-a')).toBeVisible()
  expect(within(dialog).getByText('gpt-4o-gizmo-b')).toBeVisible()

  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )
  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'gpt-4o-gizmo-a',
          expected_version: 'alias-v1',
          pricing: {
            ModelPrice: 2,
            'billing_setting.billing_mode': 'tiered_expr',
            'billing_setting.billing_expr': 'tier("base", p * 2)',
          },
        },
        {
          model_name: 'gpt-4o-gizmo-b',
          expected_version: 'alias-v2',
          pricing: {
            ModelPrice: 2,
            'billing_setting.billing_mode': 'ratio',
          },
        },
      ],
    })
  )
})

it('refreshes sibling alias effective price in table after confirming single alias selection', async () => {
  let isSaved = false
  const requestedBatches: string[][] = []
  const unrelatedRows: PricingSyncModels = {}
  for (let i = 0; i < 20; i++) {
    unrelatedRows[`z-catalog-extra-${i}`] = {
      current: { model_price: 10, billing_mode: 'ratio' },
      upstreams: { 'Upstream(7)': { model_price: 10 } },
    }
  }
  const candidates: PricingSyncModels = {
    'gpt-4o-gizmo-a': {
      current: { model_price: 1 },
      upstreams: { 'Upstream(7)': { model_price: 2 } },
    },
    'gpt-4o-gizmo-b': {
      current: { model_price: 1 },
      upstreams: { 'Upstream(7)': { model_price: 2 } },
    },
    'token-model-unrelated': {
      current: { model_ratio: 2, billing_mode: 'ratio' },
      upstreams: {
        'Upstream(7)': { model_ratio: 2, billing_mode: 'ratio' },
      },
    },
    ...unrelatedRows,
  }

  const { user, patch, post } = await renderAdjustmentSync(candidates, {
    getModelPricing: (requested) => {
      requestedBatches.push(requested)
      return {
        data: {
          success: true,
          data: {
            entries: [
              ...(requested.includes('gpt-4o-gizmo-a')
                ? [
                    {
                      model_name: 'gpt-4o-gizmo-a',
                      numeric_model_name: 'gpt-4o-gizmo-*',
                      version: 'alias-v1',
                      configured: { ModelPrice: 1 },
                      effective: { ModelPrice: isSaved ? 2 : 1 },
                    },
                  ]
                : []),
              ...(requested.includes('gpt-4o-gizmo-b')
                ? [
                    {
                      model_name: 'gpt-4o-gizmo-b',
                      numeric_model_name: 'gpt-4o-gizmo-*',
                      version: 'alias-v2',
                      configured: { ModelPrice: isSaved ? 2 : 1 },
                      effective: { ModelPrice: isSaved ? 2 : 1 },
                    },
                  ]
                : []),
            ],
            empty_version: 'empty',
            options: pricingOptions({
              ModelPrice: isSaved
                ? '{"gpt-4o-gizmo-*":2}'
                : '{"gpt-4o-gizmo-*":1}',
            }),
          },
        },
      }
    },
    getComparisonResponse: () => ({
      data: {
        success: true,
        data: {
          prices: {
            ...candidates,
            'gpt-4o-gizmo-a': {
              current: { model_price: isSaved ? 2 : 1, billing_mode: 'ratio' },
              upstreams: { 'Upstream(7)': { model_price: 2 } },
            },
            'gpt-4o-gizmo-b': {
              current: { model_price: isSaved ? 2 : 1, billing_mode: 'ratio' },
              upstreams: { 'Upstream(7)': { model_price: 2 } },
            },
            'token-model-unrelated': {
              current: { model_ratio: 2, billing_mode: 'ratio' },
              upstreams: {
                'Upstream(7)': { model_ratio: 2, billing_mode: 'ratio' },
              },
            },
          },
          differences: isSaved
            ? {}
            : {
                'gpt-4o-gizmo-a': {
                  model_price: {
                    current: 1,
                    upstreams: { 'Upstream(7)': 2 },
                    confidence: { 'Upstream(7)': true },
                  },
                },
                'gpt-4o-gizmo-b': {
                  model_price: {
                    current: 1,
                    upstreams: { 'Upstream(7)': 2 },
                    confidence: { 'Upstream(7)': true },
                  },
                },
              },
          test_results: [{ name: 'Upstream(7)', status: 'success' }],
        },
      },
    }),
  })

  await user.click(
    screen.getByRole('checkbox', {
      name: 'Show models with matching prices',
    })
  )

  const rowABefore = (await screen.findByText('gpt-4o-gizmo-a')).closest('tr')
  const rowBBefore = (await screen.findByText('gpt-4o-gizmo-b')).closest('tr')
  const rowTokenBefore = (
    await screen.findByText('token-model-unrelated')
  ).closest('tr')
  expect(
    rowABefore?.querySelector('[data-column-id="current"]')
  ).toHaveTextContent('$1')
  expect(
    rowBBefore?.querySelector('[data-column-id="current"]')
  ).toHaveTextContent('$1')
  expect(
    rowTokenBefore?.querySelector('[data-column-id="current"]')
  ).toHaveTextContent('$4')

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-b from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))

  const dialog = await screen.findByRole('alertdialog', {
    name: 'Preview price changes',
  })
  expect(within(dialog).getByText('gpt-4o-gizmo-b')).toBeVisible()
  expect(within(dialog).queryByText('gpt-4o-gizmo-a')).not.toBeInTheDocument()

  // Verify intentional lookup for selected alias occurred before preview
  expect(requestedBatches).toContainEqual(['gpt-4o-gizmo-b'])

  patch.mockImplementationOnce(async () => {
    isSaved = true
    return { data: { success: true } }
  })

  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )

  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'gpt-4o-gizmo-b',
          expected_version: 'alias-v2',
          pricing: {
            ModelPrice: 2,
            'billing_setting.billing_mode': 'ratio',
          },
        },
      ],
    })
  )
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )

  // Verify comparison was sent again with original request
  expect(post).toHaveBeenCalledTimes(2)
  expect(post).toHaveBeenLastCalledWith(
    '/api/ratio_sync/fetch',
    expect.objectContaining({
      upstreams: expect.any(Array),
      timeout: 10,
      include_unchanged: true,
    })
  )

  // Verify no name-heavy GET was sent during post-save refresh
  const intentionalIndex = requestedBatches.findIndex(
    (batch) => batch.length === 1 && batch[0] === 'gpt-4o-gizmo-b'
  )
  const postSaveBatches = requestedBatches.slice(intentionalIndex + 1)
  expect(postSaveBatches.length).toBeGreaterThan(0)
  for (const batch of postSaveBatches) {
    expect(batch.length).toBe(0)
  }

  const rowAAfter = (await screen.findByText('gpt-4o-gizmo-a')).closest('tr')
  const rowBAfter = (await screen.findByText('gpt-4o-gizmo-b')).closest('tr')
  const rowTokenAfter = (
    await screen.findByText('token-model-unrelated')
  ).closest('tr')
  await waitFor(() => {
    expect(
      rowAAfter?.querySelector('[data-column-id="current"]')
    ).toHaveTextContent('$2')
  })
  expect(
    rowBAfter?.querySelector('[data-column-id="current"]')
  ).toHaveTextContent('$2')
  // Verify token-only model remains identical without injected default ratios
  expect(
    rowTokenAfter?.querySelector('[data-column-id="current"]')
  ).toHaveTextContent('$4')
  expect(
    within(rowTokenAfter as HTMLElement).queryByText(/Completion|Output/)
  ).not.toBeInTheDocument()
})

it('retains low confidence warning when previously matching source becomes divergent on server refresh', async () => {
  let isSaved = false
  const { user, patch } = await renderAdjustmentSync(
    {
      'source-match-model': {
        current: { model_price: 2, billing_mode: 'ratio' },
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
      'target-change-model': {
        current: { model_price: 1 },
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
    },
    {
      getComparisonResponse: () => ({
        data: {
          success: true,
          data: {
            prices: {
              'source-match-model': {
                current: {
                  model_price: isSaved ? 3 : 2,
                  billing_mode: 'ratio',
                },
                upstreams: { 'Upstream(7)': { model_price: 2 } },
              },
              'target-change-model': {
                current: {
                  model_price: isSaved ? 2 : 1,
                  billing_mode: 'ratio',
                },
                upstreams: { 'Upstream(7)': { model_price: 2 } },
              },
            },
            differences: isSaved
              ? {
                  'source-match-model': {
                    model_price: {
                      current: 3,
                      upstreams: { 'Upstream(7)': 2 },
                      confidence: { 'Upstream(7)': false },
                    },
                  },
                }
              : {
                  'target-change-model': {
                    model_price: {
                      current: 1,
                      upstreams: { 'Upstream(7)': 2 },
                      confidence: { 'Upstream(7)': true },
                    },
                  },
                },
            test_results: [{ name: 'Upstream(7)', status: 'success' }],
          },
        },
      }),
    }
  )

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for target-change-model from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  const dialog = await screen.findByRole('alertdialog', {
    name: 'Preview price changes',
  })

  patch.mockImplementationOnce(async () => {
    isSaved = true
    return { data: { success: true } }
  })
  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )

  expect(await screen.findByText('source-match-model')).toBeVisible()
  expect(screen.getByText('Verify upstream pricing')).toBeVisible()
})

it('proves exactly one PATCH is sent across refresh failure and subsequent retry', async () => {
  let isSaved = false
  let shouldFailRefresh = false
  const { user, patch } = await renderAdjustmentSync(
    {
      'retry-test-model': {
        current: { model_price: 1 },
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
    },
    {
      getComparisonResponse: () => {
        if (shouldFailRefresh) {
          throw new Error('Upstream comparison failed')
        }
        return {
          data: {
            success: true,
            data: {
              prices: {
                'retry-test-model': {
                  current: {
                    model_price: isSaved ? 2 : 1,
                    billing_mode: 'ratio',
                  },
                  upstreams: { 'Upstream(7)': { model_price: 2 } },
                },
              },
              differences: isSaved
                ? {}
                : {
                    'retry-test-model': {
                      model_price: {
                        current: 1,
                        upstreams: { 'Upstream(7)': 2 },
                        confidence: { 'Upstream(7)': true },
                      },
                    },
                  },
              test_results: [{ name: 'Upstream(7)', status: 'success' }],
            },
          },
        }
      },
    }
  )

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for retry-test-model from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  const dialog = await screen.findByRole('alertdialog', {
    name: 'Preview price changes',
  })

  patch.mockImplementationOnce(async () => {
    isSaved = true
    shouldFailRefresh = true
    return { data: { success: true } }
  })
  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )

  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )

  // Verify ErrorState is displayed and retry button is present
  expect(await screen.findByText('Upstream comparison failed')).toBeVisible()
  expect(patch).toHaveBeenCalledTimes(1)

  // Click Retry
  shouldFailRefresh = false
  await user.click(screen.getByRole('button', { name: 'Retry' }))

  // Verify table refreshes with authoritative prices, and no second PATCH was called
  await waitFor(() =>
    expect(
      screen.queryByText('Upstream comparison failed')
    ).not.toBeInTheDocument()
  )
  expect(patch).toHaveBeenCalledTimes(1)
})

it('discards stale preview if selections change during delayed lookup and re-enables apply', async () => {
  let resolveLookup: (value?: unknown) => void = () => {}
  const { user, patch } = await renderAdjustmentSync(
    {
      'gpt-4o-gizmo-new': {
        current: { model_price: 1 },
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
    },
    {
      getModelPricing: (requested) => {
        if (requested.includes('gpt-4o-gizmo-new')) {
          return new Promise((resolve) => {
            resolveLookup = () =>
              resolve({
                data: {
                  success: true,
                  data: {
                    entries: [
                      {
                        model_name: 'gpt-4o-gizmo-new',
                        numeric_model_name: 'gpt-4o-gizmo-*',
                        version: 'alias-v1',
                        configured: { ModelPrice: 1 },
                        effective: { ModelPrice: 1 },
                      },
                    ],
                    empty_version: 'empty',
                    options: pricingOptions({
                      ModelPrice: '{"gpt-4o-gizmo-*":1}',
                    }),
                  },
                },
              })
          })
        }
        return {
          data: {
            success: true,
            data: {
              entries: [],
              empty_version: 'empty',
              options: pricingOptions({
                ModelPrice: '{"gpt-4o-gizmo-*":1}',
              }),
            },
          },
        }
      },
    }
  )

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-new from Upstream(7)',
    })
  )

  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))
  await user.click(screen.getByRole('button', { name: 'Clear selection' }))

  resolveLookup()

  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(patch).not.toHaveBeenCalled()

  await user.click(
    screen.getByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-new from Upstream(7)',
    })
  )
  const applyButton = screen.getByRole('button', { name: 'Apply Sync' })
  expect(applyButton).toBeEnabled()
  await user.click(applyButton)
  resolveLookup()

  const dialog = await screen.findByRole('alertdialog', {
    name: 'Preview price changes',
  })
  expect(dialog).toBeVisible()
  await user.click(
    within(dialog).getByRole('button', { name: 'Confirm Changes' })
  )
  await waitFor(() => expect(patch).toHaveBeenCalledTimes(1))
})

it('aborts preview without saving when server pricing drifted before resolution', async () => {
  const { user, patch } = await renderAdjustmentSync(
    {
      'gpt-4o-gizmo-new': {
        current: { model_price: 1 },
        upstreams: { 'Upstream(7)': { model_price: 2 } },
      },
    },
    {
      getModelPricing: (requested) => ({
        data: {
          success: true,
          data: {
            entries: requested.includes('gpt-4o-gizmo-new')
              ? [
                  {
                    model_name: 'gpt-4o-gizmo-new',
                    numeric_model_name: 'gpt-4o-gizmo-*',
                    version: 'alias-v2',
                    configured: { ModelPrice: 3 },
                    effective: { ModelPrice: 3 },
                  },
                ]
              : [],
            empty_version: 'empty',
            options: pricingOptions({
              ModelPrice: requested.includes('gpt-4o-gizmo-new')
                ? '{"gpt-4o-gizmo-*":3}'
                : '{"gpt-4o-gizmo-*":1}',
            }),
          },
        },
      }),
    }
  )

  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Select price for gpt-4o-gizmo-new from Upstream(7)',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Apply Sync' }))

  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(patch).not.toHaveBeenCalled()
})
