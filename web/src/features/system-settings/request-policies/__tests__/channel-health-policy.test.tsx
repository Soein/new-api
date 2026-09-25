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
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { ChannelHealthSection } from '../channel-health-section'
import { defaultRequestPolicySettings, type HealthSettings } from '../defaults'

const nonDefaultHealthSettings: HealthSettings = {
  ...defaultRequestPolicySettings,
  FrtBreakerEnabled: true,
  FrtBreakerThresholdSec: 20,
  FrtBreakerStrikes: 5,
  FrtBreakerWindowSec: 120,
  FrtBreakerCooldownSec: 180,
  FrtBreakerHalfOpenEnabled: true,
  FrtBreakerHalfOpenWindowSec: 90,
  FrtBreakerHalfOpenStrikes: 2,
  FrtBreakerHalfOpenSweepSec: 45,
}

let client: QueryClient

function Fixture(props: { defaultValues: HealthSettings }) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <ChannelHealthSection defaultValues={props.defaultValues} />
      </SettingsPageProvider>
    </>
  )
}

function renderHealthSection(
  values: HealthSettings = nonDefaultHealthSettings
) {
  return render(
    <QueryClientProvider client={client}>
      <Fixture defaultValues={values} />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  vi.spyOn(api, 'patch').mockResolvedValue({
    data: { success: true, data: { options: {} } },
  })
})

afterEach(() => {
  cleanup()
  client.clear()
  vi.restoreAllMocks()
})

describe('channel health request policy - production FRT breaker', () => {
  it('displays non-default values for all 9 FRT controls correctly', () => {
    renderHealthSection()

    expect(
      screen.getByRole('switch', { name: 'Production FRT breaker' })
    ).toBeChecked()
    expect(
      screen.getByRole('spinbutton', {
        name: 'First response threshold (seconds)',
      })
    ).toHaveValue(20)
    expect(
      screen.getByRole('spinbutton', { name: 'Strike count' })
    ).toHaveValue(5)
    expect(
      screen.getByRole('spinbutton', { name: 'Strike window (seconds)' })
    ).toHaveValue(120)
    expect(
      screen.getByRole('spinbutton', { name: 'Cooldown (seconds)' })
    ).toHaveValue(180)
    expect(
      screen.getByRole('switch', { name: 'Half-open recovery' })
    ).toBeChecked()
    expect(
      screen.getByRole('spinbutton', { name: 'Half-open window (seconds)' })
    ).toHaveValue(90)
    expect(
      screen.getByRole('spinbutton', { name: 'Half-open strike count' })
    ).toHaveValue(2)
    expect(
      screen.getByRole('spinbutton', {
        name: 'Half-open scan interval (seconds)',
      })
    ).toHaveValue(45)
  })

  it('saves only changed FRT threshold and half-open sweep interval as string values', async () => {
    renderHealthSection()

    const thresholdInput = screen.getByRole('spinbutton', {
      name: 'First response threshold (seconds)',
    })
    const sweepInput = screen.getByRole('spinbutton', {
      name: 'Half-open scan interval (seconds)',
    })

    fireEvent.change(thresholdInput, { target: { value: '25' } })
    fireEvent.change(sweepInput, { target: { value: '60' } })

    const saveButton = screen.getByRole('button', { name: 'Save Changes' })
    fireEvent.click(saveButton)

    await waitFor(() => expect(api.patch).toHaveBeenCalledTimes(1))
    const payload = vi.mocked(api.patch).mock.calls[0][1] as {
      options: Record<string, string>
    }
    expect(payload.options).toEqual({
      FrtBreakerThresholdSec: '25',
      FrtBreakerHalfOpenSweepSec: '60',
    })
  })

  it('prevents saving when half-open scan interval is less than 5', async () => {
    renderHealthSection()

    const sweepInput = screen.getByRole('spinbutton', {
      name: 'Half-open scan interval (seconds)',
    })

    fireEvent.change(sweepInput, { target: { value: '4' } })

    const saveButton = screen.getByRole('button', { name: 'Save Changes' })
    fireEvent.click(saveButton)

    await waitFor(() =>
      expect(sweepInput).toHaveAttribute('aria-invalid', 'true')
    )
    expect(api.patch).not.toHaveBeenCalled()
  })
})
