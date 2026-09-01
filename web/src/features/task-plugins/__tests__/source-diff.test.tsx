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

import { render, screen } from '@testing-library/react'
import { describe, test, vi } from 'vitest'

import { SourceDiff } from '../components/source-diff'
import {
  computeSourceDiff,
  MAX_DIFF_CELLS,
  MAX_DIFF_CHARS,
  MAX_DIFF_LINES,
} from '../lib/source-diff'

describe('computeSourceDiff algorithm and budgets', () => {
  test('correctly computes diff for ordinary small inputs', () => {
    const before = 'line 1\nline 2\nline 3'
    const after = 'line 1\nline 2 modified\nline 3\nline 4'
    const result = computeSourceDiff(before, after)
    assert.equal(result.tooLarge, false)
    if (!result.tooLarge) {
      assert.deepEqual(
        result.lines.map((l) => ({ kind: l.kind, text: l.text })),
        [
          { kind: 'same', text: 'line 1' },
          { kind: 'removed', text: 'line 2' },
          { kind: 'added', text: 'line 2 modified' },
          { kind: 'same', text: 'line 3' },
          { kind: 'added', text: 'line 4' },
        ]
      )
    }
  })

  test('efficiently handles identical inputs within line budget', () => {
    const lines = Array.from({ length: 1000 }, (_, i) => `line ${i}`).join('\n')
    const result = computeSourceDiff(lines, lines)
    assert.equal(result.tooLarge, false)
    if (!result.tooLarge) {
      assert.equal(result.lines.length, 1000)
      assert.ok(result.lines.every((l) => l.kind === 'same'))
    }
  })

  test('guards against identical inputs exceeding line budget', () => {
    const lines = Array.from(
      { length: MAX_DIFF_LINES + 100 },
      (_, i) => `line ${i}`
    ).join('\n')
    const result = computeSourceDiff(lines, lines)
    assert.equal(result.tooLarge, true)
    if (result.tooLarge) {
      assert.equal(result.lineCountBefore, MAX_DIFF_LINES + 1)
      assert.equal(result.lineCountAfter, MAX_DIFF_LINES + 1)
    }
  })

  test('guards against newline-dense inputs near character budget without calling split', () => {
    const splitSpy = vi.spyOn(String.prototype, 'split')
    try {
      const dense = '\n'.repeat(100_000)
      const result = computeSourceDiff(dense, dense)
      assert.equal(result.tooLarge, true)
      if (result.tooLarge) {
        assert.equal(result.lineCountBefore, MAX_DIFF_LINES + 1)
        assert.equal(result.lineCountAfter, MAX_DIFF_LINES + 1)
      }
      assert.equal(splitSpy.mock.calls.length, 0)
    } finally {
      splitSpy.mockRestore()
    }
  })

  test('guards against inputs exceeding maxChars budget without calling split', () => {
    const splitSpy = vi.spyOn(String.prototype, 'split')
    try {
      const huge = 'a\n'.repeat(MAX_DIFF_CHARS)
      const result = computeSourceDiff(huge, 'b')
      assert.equal(result.tooLarge, true)
      assert.equal(splitSpy.mock.calls.length, 0)
    } finally {
      splitSpy.mockRestore()
    }
  })

  test('guards against inputs with common prefix/suffix exceeding configured maxLines', () => {
    const prefix = Array.from({ length: 50 }, (_, i) => `prefix-${i}`)
    const suffix = Array.from({ length: 50 }, (_, i) => `suffix-${i}`)
    const before = [...prefix, 'mid-before', ...suffix].join('\n')
    const after = [...prefix, 'mid-after', ...suffix].join('\n')

    // Total lines is 101, which exceeds maxLines: 100
    const result = computeSourceDiff(before, after, { maxLines: 100 })
    assert.equal(result.tooLarge, true)
    if (result.tooLarge) {
      assert.equal(result.lineCountBefore, 101)
      assert.equal(result.lineCountAfter, 101)
    }
  })

  test('guards against combined added and removed lines exceeding line budget when individual inputs are within budget', () => {
    // Both inputs individually have 60 lines (<= maxLines: 100), but have no lines in common.
    // The resulting added + removed output would be 120 lines (> maxLines: 100).
    const left = Array.from({ length: 60 }, (_, i) => `left-${i}`).join('\n')
    const right = Array.from({ length: 60 }, (_, i) => `right-${i}`).join('\n')

    const result = computeSourceDiff(left, right, { maxLines: 100 })
    assert.equal(result.tooLarge, true)
    if (result.tooLarge) {
      assert.equal(result.lineCountBefore, 60)
      assert.equal(result.lineCountAfter, 60)
    }
  })

  test('allows diff when common lines bring total rendered diff within budget', () => {
    // left: 60 lines, right: 60 lines (individual <= 100)
    // 30 common lines + 30 removed + 30 added = 90 total diff lines (<= 100)
    const common = Array.from({ length: 30 }, (_, i) => `common-${i}`)
    const left = [
      ...common,
      ...Array.from({ length: 30 }, (_, i) => `left-${i}`),
    ].join('\n')
    const right = [
      ...common,
      ...Array.from({ length: 30 }, (_, i) => `right-${i}`),
    ].join('\n')

    const result = computeSourceDiff(left, right, { maxLines: 100 })
    assert.equal(result.tooLarge, false)
    if (!result.tooLarge) {
      assert.equal(result.lines.length, 90)
    }
  })

  test('guards against quadratic table allocation when differing lines exceed cell budget', () => {
    // 600 completely different lines on left vs 600 completely different lines on right
    // 600 * 600 = 360,000 cells > MAX_DIFF_CELLS (250,000)
    assert.ok(600 * 600 > MAX_DIFF_CELLS)
    const left = Array.from({ length: 600 }, (_, i) => `left-${i}`).join('\n')
    const right = Array.from({ length: 600 }, (_, i) => `right-${i}`).join('\n')
    const result = computeSourceDiff(left, right)
    assert.equal(result.tooLarge, true)
    if (result.tooLarge) {
      assert.equal(result.lineCountBefore, 600)
      assert.equal(result.lineCountAfter, 600)
    }
  })

  test('guards against lines exceeding MAX_DIFF_LINES', () => {
    const left = Array.from(
      { length: MAX_DIFF_LINES + 10 },
      (_, i) => `line-${i}`
    ).join('\n')
    const right = 'single line'
    const result = computeSourceDiff(left, right)
    assert.equal(result.tooLarge, true)
    if (result.tooLarge) {
      assert.equal(result.lineCountBefore, MAX_DIFF_LINES + 1)
      assert.equal(result.lineCountAfter, 1)
    }

    const resultAfter = computeSourceDiff(right, left)
    assert.equal(resultAfter.tooLarge, true)
    if (resultAfter.tooLarge) {
      assert.equal(resultAfter.lineCountBefore, 1)
      assert.equal(resultAfter.lineCountAfter, MAX_DIFF_LINES + 1)
    }
  })
})

describe('SourceDiff component rendering', () => {
  test('renders ordinary diff with added and removed indicators', () => {
    render(<SourceDiff before={'hello\nworld'} after={'hello\nthere\nworld'} />)
    const region = screen.getByRole('region', { name: /source diff/i })
    assert.ok(region)
    assert.ok(screen.getByText(/\+ there/))
  })

  test('renders accessible fallback when diff exceeds budget', () => {
    const left = Array.from({ length: 600 }, (_, i) => `left-${i}`).join('\n')
    const right = Array.from({ length: 600 }, (_, i) => `right-${i}`).join('\n')
    render(<SourceDiff before={left} after={right} />)
    const region = screen.getByRole('region', { name: /source diff/i })
    assert.ok(region)
    assert.ok(screen.getByText(/too large to display inline/i))
    assert.ok(screen.getByText(/600/))
  })

  test('renders accessible fallback when identical inputs exceed line budget', () => {
    const lines = Array.from(
      { length: MAX_DIFF_LINES + 50 },
      (_, i) => `line ${i}`
    ).join('\n')
    render(<SourceDiff before={lines} after={lines} />)
    const region = screen.getByRole('region', { name: /source diff/i })
    assert.ok(region)
    assert.ok(screen.getByText(/too large to display inline/i))
    assert.ok(screen.getByText(new RegExp(`${MAX_DIFF_LINES + 1}`)))
  })
})
