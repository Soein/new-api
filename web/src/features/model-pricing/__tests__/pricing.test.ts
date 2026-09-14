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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError, type AxiosAdapter } from 'axios'
import React from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import {
  buildPricingChanges,
  preparePricingChanges,
  resolvePricingSnapshot,
  saveModelPricing,
  type ModelPricingConfig,
  type ModelPricingEntry,
} from '../api'
import { ModelPricingPanel } from '../model-pricing-panel'
import {
  applyPriceSyncSelections,
  applyPricingDraft,
  pricingDisplayOptions,
  pricingFromDraft,
  pricingOptions,
  pricingRow,
} from '../pricing'

const originalAdapter = api.defaults.adapter

describe('shared model pricing', () => {
  it('preserves explicit zero prices and cache-write configuration', () => {
    expect(
      pricingFromDraft({
        name: 'example',
        billingMode: 'per-token',
        ratio: '0',
        completionRatio: '2',
        cacheRatio: '0',
        createCacheRatio: '1.25',
      })
    ).toEqual({
      'billing_setting.billing_mode': 'ratio',
      ModelRatio: 0,
      CompletionRatio: 2,
      CacheRatio: 0,
      CreateCacheRatio: 1.25,
    })
    expect(
      pricingFromDraft({
        name: 'example',
        billingMode: 'per-request',
        price: '',
      })
    ).not.toHaveProperty('ModelPrice')
    expect(
      pricingFromDraft({
        name: 'example',
        billingMode: 'per-request',
        price: '0',
      })
    ).toHaveProperty('ModelPrice', 0)
  })

  it('keeps token and task expressions intact through both editing and sync', () => {
    for (const expression of [
      'len <= 200000 ? tier("short", p * 2 + cr * 0.2 + cc * 2.5) : tier("long", p * 4)',
      'tier("base", u("seconds") * 0.4)',
    ]) {
      const values = {
        'billing_setting.billing_mode': 'tiered_expr',
        'billing_setting.billing_expr': expression,
        ModelRatio: 1,
      }
      expect(pricingFromDraft(pricingRow('example', values))).toEqual(values)
    }
  })

  it('does not persist another model’s built-in display expression when editing one price', () => {
    const options = pricingOptions({
      ModelPrice: '{"edited":1}',
      BillingMode: '{"builtin":"tiered_expr"}',
      BillingExpr: '{"builtin":"tier(\\"base\\", p * 2)"}',
    })
    const snapshot: ModelPricingConfig = {
      options,
      empty_version: 'empty',
      entries: [
        {
          model_name: 'edited',
          version: 'v1',
          configured: { ModelPrice: 1 },
          effective: { ModelPrice: 1 },
        },
        {
          model_name: 'builtin',
          version: 'empty',
          configured: {},
          effective: {
            'billing_setting.billing_mode': 'tiered_expr',
            'billing_setting.billing_expr': 'tier("base", p * 2)',
          },
        },
      ],
    }
    const after = applyPricingDraft(options, {
      name: 'edited',
      billingMode: 'per-request',
      price: '2',
    })
    expect(buildPricingChanges(snapshot, options, after)).toEqual([
      {
        model_name: 'edited',
        expected_version: 'v1',
        pricing: { ModelPrice: 2, 'billing_setting.billing_mode': 'ratio' },
      },
    ])
  })

  it('clears conflicting expression settings when a fixed price is selected for sync', () => {
    const options = pricingOptions({
      ModelRatio: '{"example":1}',
      CreateCacheRatio: '{"example":1.25}',
      BillingMode: '{"example":"tiered_expr"}',
      BillingExpr: '{"example":"tier(\\"base\\", p * 2)"}',
    })
    const after = applyPriceSyncSelections(options, {
      example: { model_price: 0 },
    })
    expect(JSON.parse(after.ModelPrice)).toEqual({ example: 0 })
    expect(JSON.parse(after.ModelRatio)).toEqual({})
    expect(JSON.parse(after.CreateCacheRatio)).toEqual({})
    expect(JSON.parse(after['billing_setting.billing_expr'])).toEqual({})
    expect(JSON.parse(after['billing_setting.billing_mode'])).toEqual({
      example: 'ratio',
    })
  })

  it('rejects invalid prices instead of silently coercing them', () => {
    for (const price of ['-1', 'NaN', 'Infinity', 'invalid']) {
      expect(() =>
        pricingFromDraft({ name: 'example', billingMode: 'per-request', price })
      ).toThrow()
    }
  })

  afterEach(() => {
    api.defaults.adapter = originalAdapter
    vi.restoreAllMocks()
    useAuthStore.getState().auth.reset('idle')
  })

  it('displays shared scope notice and canonical configured pricing for alias models, with warning on reset', async () => {
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'admin',
      role: ROLE.SUPER_ADMIN,
    })
    const entry: ModelPricingEntry = {
      model_name: 'gpt-4o-gizmo-custom',
      numeric_model_name: 'gpt-4o-gizmo-*',
      version: 'v-canonical',
      configured: {
        ModelRatio: 2.5,
        CompletionRatio: 3,
        CacheRatio: 0.5,
      },
      effective: {
        ModelRatio: 2.5,
        CompletionRatio: 3,
        CacheRatio: 0.5,
      },
    }
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelRatio: '{"gpt-4o-gizmo-custom":99,"gpt-4o-gizmo-*":2.5}',
        CompletionRatio: '{"gpt-4o-gizmo-*":3}',
        CacheRatio: '{"gpt-4o-gizmo-custom":0.5}',
      }),
      empty_version: 'empty',
      entries: [entry],
    }
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: snapshot },
    })
    const patchSpy = vi.spyOn(api, 'patch').mockResolvedValue({
      data: { success: true },
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const user = userEvent.setup()
    render(
      React.createElement(
        QueryClientProvider,
        { client },
        React.createElement(ModelPricingPanel, {
          modelName: 'gpt-4o-gizmo-custom',
        })
      )
    )

    const notice = await screen.findByText(
      /all models matching.*gpt-4o-gizmo-\*/i
    )
    expect(notice).toBeVisible()
    expect(notice).toHaveTextContent(/only to this model/i)

    const currentBilling = screen.getByRole('region', {
      name: /Current Billing/i,
    })
    expect(within(currentBilling).getByText('Input')).toBeVisible()
    expect(within(currentBilling).getByText('5')).toBeVisible()
    expect(within(currentBilling).getByText('Output')).toBeVisible()
    expect(within(currentBilling).getByText('15')).toBeVisible()
    expect(within(currentBilling).getByText(/USD/i)).toBeVisible()
    expect(screen.getByDisplayValue('5')).toBeVisible()

    await user.click(
      screen.getByRole('button', { name: /Restore default pricing/i })
    )
    const alertDialog = await screen.findByRole('alertdialog')
    expect(alertDialog).toBeInTheDocument()
    expect(alertDialog).toHaveTextContent(
      /all models matching.*gpt-4o-gizmo-\*/i
    )

    await user.click(
      within(alertDialog).getByRole('button', { name: /Restore defaults/i })
    )
    await waitFor(() => expect(patchSpy).toHaveBeenCalled())
    expect(patchSpy).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'gpt-4o-gizmo-custom',
          expected_version: 'v-canonical',
          pricing: {},
          reset: true,
        },
      ],
    })
  })

  it('does not display shared scope notice when numeric_model_name matches model_name', async () => {
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'admin',
      role: ROLE.SUPER_ADMIN,
    })
    const entry: ModelPricingEntry = {
      model_name: 'gpt-4o-gizmo-*',
      numeric_model_name: 'gpt-4o-gizmo-*',
      version: 'v1',
      configured: { ModelPrice: 0.03 },
      effective: { ModelPrice: 0.03 },
    }
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({ ModelPrice: '{"gpt-4o-gizmo-*":0.03}' }),
      empty_version: 'empty',
      entries: [entry],
    }
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: snapshot },
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const user = userEvent.setup()
    render(
      React.createElement(
        QueryClientProvider,
        { client },
        React.createElement(ModelPricingPanel, {
          modelName: 'gpt-4o-gizmo-*',
        })
      )
    )

    expect(
      await screen.findByRole('button', { name: /Restore default pricing/i })
    ).toBeVisible()
    expect(screen.queryByText(/all models matching/i)).not.toBeInTheDocument()

    await user.click(
      screen.getByRole('button', { name: /Restore default pricing/i })
    )
    const alertDialog = await screen.findByRole('alertdialog')
    expect(alertDialog).not.toHaveTextContent(/all models matching/i)
  })

  it('synchronizes shared alias price changes across models and retains exact field isolation', () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"gpt-4o-gizmo-a":1,"gpt-4o-gizmo-b":1}',
        CacheRatio: '{"gpt-4o-gizmo-b":0.2}',
      }),
      empty_version: 'empty',
      entries: [
        {
          model_name: 'gpt-4o-gizmo-a',
          numeric_model_name: 'gpt-4o-gizmo-*',
          version: 'v1',
          configured: { ModelPrice: 1 },
          effective: { ModelPrice: 1 },
        },
        {
          model_name: 'gpt-4o-gizmo-b',
          numeric_model_name: 'gpt-4o-gizmo-*',
          version: 'v2',
          configured: { ModelPrice: 1, CacheRatio: 0.2 },
          effective: { ModelPrice: 1, CacheRatio: 0.2 },
        },
      ],
    }
    const before = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-a":1,"gpt-4o-gizmo-b":1}',
      CacheRatio: '{"gpt-4o-gizmo-b":0.2}',
    })
    const after = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-a":2,"gpt-4o-gizmo-b":1}',
      CacheRatio: '{"gpt-4o-gizmo-b":0.5}',
    })

    const changes = buildPricingChanges(snapshot, before, after)
    expect(changes).toEqual([
      {
        model_name: 'gpt-4o-gizmo-a',
        expected_version: 'v1',
        pricing: { ModelPrice: 2 },
      },
      {
        model_name: 'gpt-4o-gizmo-b',
        expected_version: 'v2',
        pricing: { ModelPrice: 2, CacheRatio: 0.5 },
      },
    ])
  })

  it('rejects conflicting explicit shared alias changes across models in buildPricingChanges', () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"gpt-4o-gizmo-a":1,"gpt-4o-gizmo-b":1}',
      }),
      empty_version: 'empty',
      entries: [
        {
          model_name: 'gpt-4o-gizmo-a',
          numeric_model_name: 'gpt-4o-gizmo-*',
          version: 'v1',
          configured: { ModelPrice: 1 },
          effective: { ModelPrice: 1 },
        },
        {
          model_name: 'gpt-4o-gizmo-b',
          numeric_model_name: 'gpt-4o-gizmo-*',
          version: 'v2',
          configured: { ModelPrice: 1 },
          effective: { ModelPrice: 1 },
        },
      ],
    }
    const before = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-a":1,"gpt-4o-gizmo-b":1}',
    })
    const after = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-a":2,"gpt-4o-gizmo-b":3}',
    })

    expect(() => buildPricingChanges(snapshot, before, after)).toThrow()
  })

  it('projects canonical configured prices and removes stale raw values without mutating source snapshot options', () => {
    const rawOptions = pricingOptions({
      ModelRatio: '{"gpt-4o-gizmo-custom":99,"gpt-4o-gizmo-*":2.5}',
      CompletionRatio: '{"gpt-4o-gizmo-*":3}',
      ModelPrice: '{"gpt-4o-gizmo-custom":99}',
      CacheRatio: '{"gpt-4o-gizmo-custom":0.5}',
      BillingExpr: '{"builtin":"tier(\\"base\\", p * 2)"}',
    })
    const snapshot: ModelPricingConfig = {
      options: rawOptions,
      empty_version: 'empty',
      entries: [
        {
          model_name: 'gpt-4o-gizmo-custom',
          numeric_model_name: 'gpt-4o-gizmo-*',
          version: 'v1',
          configured: {
            ModelRatio: 2.5,
            CompletionRatio: 3,
          },
          effective: {
            ModelRatio: 2.5,
            CompletionRatio: 3,
          },
        },
      ],
    }

    const projected = pricingDisplayOptions(snapshot)
    expect(JSON.parse(projected.ModelRatio)).toEqual({
      'gpt-4o-gizmo-custom': 2.5,
      'gpt-4o-gizmo-*': 2.5,
    })
    expect(JSON.parse(projected.CompletionRatio)).toEqual({
      'gpt-4o-gizmo-custom': 3,
      'gpt-4o-gizmo-*': 3,
    })
    expect(JSON.parse(projected.ModelPrice)).toEqual({})
    expect(JSON.parse(projected.CacheRatio)).toEqual({
      'gpt-4o-gizmo-custom': 0.5,
    })
    expect(JSON.parse(projected['billing_setting.billing_expr'])).toEqual({
      builtin: 'tier("base", p * 2)',
    })
    expect(JSON.parse(snapshot.options.ModelRatio)).toEqual({
      'gpt-4o-gizmo-custom': 99,
      'gpt-4o-gizmo-*': 2.5,
    })
    expect(JSON.parse(snapshot.options.ModelPrice)).toEqual({
      'gpt-4o-gizmo-custom': 99,
    })
  })

  it('fetches missing alias entry and uses its backend version and numeric alias', async () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"gpt-4o-gizmo-*":1}',
      }),
      empty_version: 'v-empty',
      entries: [],
    }
    const before = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-custom":1}',
    })
    const after = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-custom":2}',
    })

    const getSpy = vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          options: snapshot.options,
          empty_version: 'v-empty',
          entries: [
            {
              model_name: 'gpt-4o-gizmo-custom',
              numeric_model_name: 'gpt-4o-gizmo-*',
              version: 'v-fresh',
              configured: { ModelPrice: 1 },
              effective: { ModelPrice: 1 },
            },
          ],
        },
      },
    })

    const changes = await preparePricingChanges(snapshot, before, after)
    expect(getSpy).toHaveBeenCalledWith(
      '/api/option/model_pricing',
      expect.objectContaining({
        params: expect.any(URLSearchParams),
      })
    )
    expect(changes).toEqual([
      {
        model_name: 'gpt-4o-gizmo-custom',
        expected_version: 'v-fresh',
        pricing: { ModelPrice: 2 },
      },
    ])
  })

  it('rejects save when missing alias shared baseline changed concurrently', async () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"gpt-4o-gizmo-*":1}',
      }),
      empty_version: 'v-empty',
      entries: [],
    }
    const before = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-custom":1}',
    })
    const after = pricingOptions({
      ModelPrice: '{"gpt-4o-gizmo-custom":2}',
    })

    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          options: snapshot.options,
          empty_version: 'v-empty',
          entries: [
            {
              model_name: 'gpt-4o-gizmo-custom',
              numeric_model_name: 'gpt-4o-gizmo-*',
              version: 'v-fresh',
              configured: { ModelPrice: 1.5 },
              effective: { ModelPrice: 1.5 },
            },
          ],
        },
      },
    })

    await expect(
      preparePricingChanges(snapshot, before, after)
    ).rejects.toThrow()
  })

  it('rejects save when missing entry concurrent plugin pricing changed, but allows when unchanged', async () => {
    const key = 'billing_setting.plugin_billing_expr'
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({}),
      empty_version: 'v-empty',
      entries: [],
    }
    const before = snapshot.options
    // User A edits unconfigured model new-shared and adds alpha plugin pricing
    const after = applyPricingDraft(before, {
      name: 'new-shared',
      pluginBillingExpr: { alpha: 'u("seconds")' },
    })

    // User B concurrently saved beta plugin pricing on server
    vi.spyOn(api, 'get').mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          options: snapshot.options,
          empty_version: 'v-empty',
          entries: [
            {
              model_name: 'new-shared',
              version: 'v-fresh-after-beta-save',
              configured: {
                [key]: { beta: 'u("credits")' },
              },
              effective: {},
            },
          ],
        },
      },
    })

    // User A's save resolution must reject concurrent beta addition instead of silently clobbering it
    await expect(
      preparePricingChanges(snapshot, before, after)
    ).rejects.toThrow()

    // When server entry has unchanged plugin pricing matching snapshot baseline, save succeeds
    vi.spyOn(api, 'get').mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          options: snapshot.options,
          empty_version: 'v-empty',
          entries: [
            {
              model_name: 'new-shared',
              version: 'v-fresh',
              configured: {},
              effective: {},
            },
          ],
        },
      },
    })

    const changes = await preparePricingChanges(snapshot, before, after)
    expect(changes).toEqual([
      {
        model_name: 'new-shared',
        expected_version: 'v-fresh',
        pricing: {
          'billing_setting.billing_mode': 'ratio',
          [key]: { alpha: 'u("seconds")' },
        },
      },
    ])

    // Key order variations without semantic changes are safely permitted
    const baselineWithOptions: ModelPricingConfig = {
      options: pricingOptions({
        [key]: JSON.stringify({
          'alpha::new-shared': 'u("seconds")',
          'beta::new-shared': 'u("credits")',
        }),
      }),
      empty_version: 'v-empty',
      entries: [],
    }
    const beforeWithPlugins = baselineWithOptions.options
    const afterReordered = applyPricingDraft(beforeWithPlugins, {
      name: 'new-shared',
      pluginBillingExpr: {
        alpha: 'u("seconds")',
        beta: 'u("credits")',
        gamma: '1',
      },
    })

    vi.spyOn(api, 'get').mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          options: baselineWithOptions.options,
          empty_version: 'v-empty',
          entries: [
            {
              model_name: 'new-shared',
              version: 'v-reordered',
              configured: {
                [key]: {
                  beta: 'u("credits")',
                  alpha: 'u("seconds")',
                },
              },
              effective: {},
            },
          ],
        },
      },
    })

    const changesReordered = await preparePricingChanges(
      baselineWithOptions,
      beforeWithPlugins,
      afterReordered
    )
    expect(changesReordered).toEqual([
      {
        model_name: 'new-shared',
        expected_version: 'v-reordered',
        pricing: {
          'billing_setting.billing_mode': 'ratio',
          [key]: {
            alpha: 'u("seconds")',
            beta: 'u("credits")',
            gamma: '1',
          },
        },
      },
    ])
  })

  it('does not refresh already-known entry versions during preparation', async () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"known-model":1}',
      }),
      empty_version: 'v-empty',
      entries: [
        {
          model_name: 'known-model',
          version: 'v-known',
          configured: { ModelPrice: 1 },
          effective: { ModelPrice: 1 },
        },
      ],
    }
    const before = pricingOptions({
      ModelPrice: '{"known-model":1}',
    })
    const after = pricingOptions({
      ModelPrice: '{"known-model":2}',
    })

    const getSpy = vi.spyOn(api, 'get')

    const changes = await preparePricingChanges(snapshot, before, after)
    expect(getSpy).not.toHaveBeenCalled()
    expect(changes).toEqual([
      {
        model_name: 'known-model',
        expected_version: 'v-known',
        pricing: { ModelPrice: 2 },
      },
    ])
  })

  it('handles explicit zero and deletion of shared and exact fields in buildPricingChanges', () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"model-a":1}',
        CacheRatio: '{"model-a":0.5}',
      }),
      empty_version: 'v-empty',
      entries: [
        {
          model_name: 'model-a',
          version: 'v1',
          configured: { ModelPrice: 1, CacheRatio: 0.5 },
          effective: { ModelPrice: 1, CacheRatio: 0.5 },
        },
      ],
    }
    const before = pricingOptions({
      ModelPrice: '{"model-a":1}',
      CacheRatio: '{"model-a":0.5}',
    })
    const after = pricingOptions({
      ModelPrice: '{"model-a":0}',
      CacheRatio: '{}',
    })

    const changes = buildPricingChanges(snapshot, before, after)
    expect(changes).toEqual([
      {
        model_name: 'model-a',
        expected_version: 'v1',
        pricing: { ModelPrice: 0 },
      },
    ])
  })

  it('propagates backend conflict rejection when saving model pricing with conflicting alias writes', async () => {
    const message = 'Conflicting pricing for shared alias numeric_model: gpt-4'
    const adapter: AxiosAdapter = async (config) => {
      throw new AxiosError(
        'Request failed with status code 409',
        'ERR_BAD_REQUEST',
        config,
        undefined,
        {
          data: { success: false, message },
          status: 409,
          statusText: 'Conflict',
          headers: {},
          config,
        }
      )
    }
    api.defaults.adapter = adapter
    await expect(
      saveModelPricing([
        {
          model_name: 'gpt-4-alias-1',
          expected_version: 'v1',
          pricing: { ModelRatio: 2 },
        },
        {
          model_name: 'gpt-4-alias-2',
          expected_version: 'v1',
          pricing: { ModelRatio: 3 },
        },
      ])
    ).rejects.toThrow(
      'Conflicting pricing for shared alias numeric_model: gpt-4'
    )
  })

  it('switches missing alias from inherited fixed price to token mode during sync', async () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"gpt-4o-gizmo-*":1}',
      }),
      empty_version: 'v-empty',
      entries: [],
    }
    const resolutions = {
      'gpt-4o-gizmo-new': { model_ratio: 2 },
    }

    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          options: snapshot.options,
          empty_version: 'v-empty',
          entries: [
            {
              model_name: 'gpt-4o-gizmo-new',
              numeric_model_name: 'gpt-4o-gizmo-*',
              version: 'v-new',
              configured: { ModelPrice: 1 },
              effective: { ModelPrice: 1 },
            },
          ],
        },
      },
    })

    const resolved = await resolvePricingSnapshot(
      snapshot,
      Object.keys(resolutions)
    )
    const before = pricingDisplayOptions(resolved)
    const after = applyPriceSyncSelections(before, resolutions)
    const changes = buildPricingChanges(resolved, before, after)

    expect(changes).toEqual([
      {
        model_name: 'gpt-4o-gizmo-new',
        expected_version: 'v-new',
        pricing: {
          ModelRatio: 2,
          'billing_setting.billing_mode': 'ratio',
        },
      },
    ])
    expect(changes[0].pricing).not.toHaveProperty('ModelPrice')
  })

  it('syncing one alias to expression does not erase sibling numeric fixed price', async () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"gpt-4o-gizmo-*":1}',
      }),
      empty_version: 'v-empty',
      entries: [
        {
          model_name: 'gpt-4o-gizmo-a',
          numeric_model_name: 'gpt-4o-gizmo-*',
          version: 'v-a',
          configured: { ModelPrice: 1 },
          effective: { ModelPrice: 1 },
        },
        {
          model_name: 'gpt-4o-gizmo-b',
          numeric_model_name: 'gpt-4o-gizmo-*',
          version: 'v-b',
          configured: { ModelPrice: 1 },
          effective: { ModelPrice: 1 },
        },
      ],
    }
    const resolutions = {
      'gpt-4o-gizmo-a': {
        billing_expr: 'tier("base", p * 2)',
      },
    }

    const resolved = await resolvePricingSnapshot(
      snapshot,
      Object.keys(resolutions)
    )
    const before = pricingDisplayOptions(resolved)
    const after = applyPriceSyncSelections(before, resolutions)
    const changes = buildPricingChanges(resolved, before, after)

    expect(JSON.parse(after.ModelPrice)).toEqual({
      'gpt-4o-gizmo-a': 1,
      'gpt-4o-gizmo-b': 1,
      'gpt-4o-gizmo-*': 1,
    })
    expect(changes).toEqual([
      {
        model_name: 'gpt-4o-gizmo-a',
        expected_version: 'v-a',
        pricing: {
          ModelPrice: 1,
          'billing_setting.billing_mode': 'tiered_expr',
          'billing_setting.billing_expr': 'tier("base", p * 2)',
        },
      },
    ])
  })

  it('rejects resolvePricingSnapshot when stale missing-alias CAS baseline changed concurrently', async () => {
    const snapshot: ModelPricingConfig = {
      options: pricingOptions({
        ModelPrice: '{"gpt-4o-gizmo-*":1}',
      }),
      empty_version: 'v-empty',
      entries: [],
    }

    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          options: snapshot.options,
          empty_version: 'v-empty',
          entries: [
            {
              model_name: 'gpt-4o-gizmo-new',
              numeric_model_name: 'gpt-4o-gizmo-*',
              version: 'v-fresh',
              configured: { ModelPrice: 1.5 },
              effective: { ModelPrice: 1.5 },
            },
          ],
        },
      },
    })

    await expect(
      resolvePricingSnapshot(snapshot, ['gpt-4o-gizmo-new'])
    ).rejects.toThrow()
  })

  it('throws on malformed JSON options instead of fabricating empty maps', () => {
    const snapshot: ModelPricingConfig = {
      options: {
        ...pricingOptions({}),
        ModelPrice: '{"invalid":',
      },
      empty_version: 'v-empty',
      entries: [],
    }
    expect(() => pricingDisplayOptions(snapshot)).toThrow()
  })

  it.each(['normal-model', 'constructor', '__proto__'])(
    'imports expression pricing safely for special model name %s',
    (name) => {
      const selections = Object.fromEntries([
        [name, { billing_expr: 'tier("base", p * 2)' }],
      ])
      const applied = applyPriceSyncSelections(pricingOptions({}), selections)
      const parsed = JSON.parse(applied['billing_setting.billing_expr'])
      expect(Object.hasOwn(parsed, name)).toBe(true)
      expect(parsed[name]).toBe('tier("base", p * 2)')
    }
  )
})

it('tracks nested provider prices by model, preserves :: in model names, and ignores key order', () => {
  const key = 'billing_setting.plugin_billing_expr'
  const before = pricingOptions({
    [key]: JSON.stringify({
      'alpha::shared::model': 'u("seconds")',
      'beta::shared::model': 'u("credits")',
      'alpha::other': '1',
    }),
  })
  const snapshot: ModelPricingConfig = {
    options: before,
    empty_version: 'empty',
    entries: [
      {
        model_name: 'shared::model',
        version: 'v1',
        configured: { [key]: { alpha: 'u("seconds")', beta: 'u("credits")' } },
        effective: {},
      },
    ],
  }
  const reordered = {
    ...before,
    [key]: JSON.stringify({
      'alpha::other': '1',
      'beta::shared::model': 'u("credits")',
      'alpha::shared::model': 'u("seconds")',
    }),
  }
  expect(buildPricingChanges(snapshot, before, reordered)).toEqual([])
  const after = {
    ...before,
    [key]: JSON.stringify({
      'beta::shared::model': 'u("credits") * 2',
      'alpha::other': '1',
    }),
  }
  expect(buildPricingChanges(snapshot, before, after)).toEqual([
    {
      model_name: 'shared::model',
      expected_version: 'v1',
      pricing: { [key]: { beta: 'u("credits") * 2' } },
    },
  ])
  const draft = pricingRow('shared::model', snapshot.entries[0].configured)
  expect(pricingFromDraft(draft)[key]).toEqual(
    snapshot.entries[0].configured[key]
  )
  expect(
    pricingRow('alpha::model', {
      ModelPrice: 0.25,
      'billing_setting.plugin_billing_expr': { beta: 'u("images")' },
    })
  ).toMatchObject({
    name: 'alpha::model',
    price: '0.25',
    billingMode: 'per-request',
  })
})

it('retains provider overrides during model-only price synchronization', () => {
  const key = 'billing_setting.plugin_billing_expr'
  const options = pricingOptions({
    [key]: JSON.stringify({ 'alpha::example': 'u("seconds") * 2' }),
  })
  const after = applyPriceSyncSelections(options, {
    example: { billing_expr: 'tier("base", u("seconds"))' },
  })
  expect(after[key]).toEqual(options[key])
})

it('commits source provider edits while preserving target providers during batch copy', () => {
  const key = 'billing_setting.plugin_billing_expr'
  const options = pricingOptions({
    [key]: JSON.stringify({ 'alpha::source': '1', 'beta::target': '2' }),
  })
  const after = applyPricingDraft(
    options,
    {
      name: 'source',
      billingMode: 'tiered_expr',
      billingExpr: 'tier("base", u("seconds"))',
      pluginBillingExpr: { alpha: '3' },
    },
    ['source', 'target']
  )
  expect(JSON.parse(after[key])).toEqual({
    'alpha::source': '3',
    'beta::target': '2',
  })
})
