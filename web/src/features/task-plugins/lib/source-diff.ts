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
export const MAX_DIFF_LINES = 2500
export const MAX_DIFF_CELLS = 250_000
export const MAX_DIFF_CHARS = 2 * 1024 * 1024

export type DiffLine = {
  id: string
  kind: 'same' | 'added' | 'removed'
  text: string
}

export type DiffResult =
  | { tooLarge: false; lines: DiffLine[] }
  | { tooLarge: true; lineCountBefore: number; lineCountAfter: number }

function countLines(text: string, limit?: number): number {
  let lines = 1
  for (let i = 0; i < text.length; i += 1) {
    if (text.charCodeAt(i) === 10) {
      lines += 1
      if (limit !== undefined && lines > limit) {
        return lines
      }
    }
  }
  return lines
}

export function computeSourceDiff(
  before: string,
  after: string,
  options: {
    maxLines?: number
    maxCells?: number
    maxChars?: number
  } = {}
): DiffResult {
  const maxLines = options.maxLines ?? MAX_DIFF_LINES
  const maxCells = options.maxCells ?? MAX_DIFF_CELLS
  const maxChars = options.maxChars ?? MAX_DIFF_CHARS

  if (before.length + after.length > maxChars) {
    const bLines = countLines(before, maxLines)
    const aLines = countLines(after, maxLines)
    return { tooLarge: true, lineCountBefore: bLines, lineCountAfter: aLines }
  }

  const beforeLines = countLines(before, maxLines)
  const afterLines = countLines(after, maxLines)

  if (beforeLines > maxLines || afterLines > maxLines) {
    return {
      tooLarge: true,
      lineCountBefore: beforeLines,
      lineCountAfter: afterLines,
    }
  }

  const left = before.split('\n')
  const right = after.split('\n')

  // 1. Trim common prefix lines
  let start = 0
  while (
    start < left.length &&
    start < right.length &&
    left[start] === right[start]
  ) {
    start += 1
  }

  // 2. Trim common suffix lines
  let leftEnd = left.length - 1
  let rightEnd = right.length - 1
  while (
    leftEnd >= start &&
    rightEnd >= start &&
    left[leftEnd] === right[rightEnd]
  ) {
    leftEnd -= 1
    rightEnd -= 1
  }

  const leftMid = left.slice(start, leftEnd + 1)
  const rightMid = right.slice(start, rightEnd + 1)

  // Guard against quadratic LCS matrix allocation on differing middle lines
  if (leftMid.length * rightMid.length > maxCells) {
    return {
      tooLarge: true,
      lineCountBefore: left.length,
      lineCountAfter: right.length,
    }
  }

  const lengths = Array.from({ length: leftMid.length + 1 }, () =>
    Array<number>(rightMid.length + 1).fill(0)
  )
  for (let i = leftMid.length - 1; i >= 0; i -= 1) {
    for (let j = rightMid.length - 1; j >= 0; j -= 1) {
      lengths[i][j] =
        leftMid[i] === rightMid[j]
          ? lengths[i + 1][j + 1] + 1
          : Math.max(lengths[i + 1][j], lengths[i][j + 1])
    }
  }

  // Enforce total rendered diff line budget before allocating DiffLine objects
  const totalDiffLines = left.length + rightMid.length - (lengths[0]?.[0] ?? 0)
  if (totalDiffLines > maxLines) {
    return {
      tooLarge: true,
      lineCountBefore: left.length,
      lineCountAfter: right.length,
    }
  }

  const result: DiffLine[] = []

  // Add common prefix lines
  for (let k = 0; k < start; k += 1) {
    result.push({ id: `pre-${k}`, kind: 'same', text: left[k] })
  }

  // Add middle LCS diff lines (removed before added for standard diff presentation)
  let i = 0
  let j = 0
  while (i < leftMid.length || j < rightMid.length) {
    if (
      i < leftMid.length &&
      j < rightMid.length &&
      leftMid[i] === rightMid[j]
    ) {
      result.push({
        id: `mid-same-${start + i}-${start + j}`,
        kind: 'same',
        text: leftMid[i],
      })
      i += 1
      j += 1
    } else if (
      i < leftMid.length &&
      (j === rightMid.length || lengths[i + 1][j] >= lengths[i][j + 1])
    ) {
      result.push({
        id: `mid-rem-${start + i}-${start + j}`,
        kind: 'removed',
        text: leftMid[i],
      })
      i += 1
    } else {
      result.push({
        id: `mid-add-${start + i}-${start + j}`,
        kind: 'added',
        text: rightMid[j],
      })
      j += 1
    }
  }

  // Add common suffix lines
  for (let k = leftEnd + 1; k < left.length; k += 1) {
    result.push({
      id: `post-${k}`,
      kind: 'same',
      text: left[k],
    })
  }

  return { tooLarge: false, lines: result }
}
