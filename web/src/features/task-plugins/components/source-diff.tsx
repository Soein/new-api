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
import { FileCode } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { computeSourceDiff } from '../lib/source-diff'

export type SourceDiffProps = { before: string; after: string }

export function SourceDiff(props: SourceDiffProps) {
  const { t } = useTranslation()
  const diffResult = computeSourceDiff(props.before, props.after)

  if (diffResult.tooLarge) {
    return (
      <div
        role='region'
        aria-label={t('Source diff')}
        className='text-muted-foreground flex flex-col items-start gap-1 rounded-md border border-dashed p-4 text-xs'
      >
        <div className='text-foreground flex items-center gap-1.5 font-medium'>
          <FileCode
            className='text-muted-foreground size-4'
            aria-hidden='true'
          />
          <span>{t('Diff is too large to display inline')}</span>
        </div>
        <p>
          {t(
            'Comparing {{beforeLines}} lines with {{afterLines}} lines exceeds the safe display budget.',
            {
              beforeLines: diffResult.lineCountBefore,
              afterLines: diffResult.lineCountAfter,
            }
          )}
        </p>
      </div>
    )
  }

  return (
    <div
      role='region'
      aria-label={t('Source diff')}
      className='max-h-96 overflow-auto rounded-md border font-mono text-xs'
    >
      {diffResult.lines.map((line) => {
        let prefix = ' '
        let color = ''
        if (line.kind === 'added') {
          prefix = '+'
          color = 'bg-green-500/10 text-green-700 dark:text-green-300'
        } else if (line.kind === 'removed') {
          prefix = '-'
          color = 'bg-red-500/10 text-red-700 dark:text-red-300'
        }
        return (
          <div key={line.id} className={`px-3 whitespace-pre ${color}`}>
            {prefix} {line.text || ' '}
          </div>
        )
      })}
    </div>
  )
}
