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
import assert from 'node:assert/strict'
import { after, describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'CustomEvent',
  'MutationObserver',
  'ResizeObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
] as const

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { DynamicPricingBreakdown } = await import('../dynamic-pricing-breakdown')
const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
  interpolation: { escapeValue: false },
})

const container = document.createElement('div')
document.body.append(container)
const root = createRoot(container)

after(() => {
  act(() => root.unmount())
  container.remove()
  domWindow.close()
})

describe('image pricing rule trace', () => {
  test('highlights the named request rule recorded by the billing log', () => {
    act(() => {
      root.render(
        <I18nextProvider i18n={i18n}>
          <DynamicPricingBreakdown
            compact
            billingExpr='v2:(tier("base", per_image(0.04))) * rule("quality_high", param("quality") == "high", 2)'
            matchedRequestRules={[{ name: 'quality_high', multiplier: 2 }]}
          />
        </I18nextProvider>
      )
    })

    const matchedRule = container.querySelector('li.border-emerald-500\\/40')
    assert.ok(matchedRule)
    assert.match(matchedRule.textContent || '', /quality_high/)
    assert.match(matchedRule.textContent || '', /Matched/)
    assert.match(matchedRule.textContent || '', /2x/)
    assert.match(container.textContent || '', /\$0\.0400/)
    assert.match(container.textContent || '', /\$\/image/)
  })
})
