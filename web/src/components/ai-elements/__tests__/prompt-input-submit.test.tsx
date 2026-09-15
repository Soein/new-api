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
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { toast } from 'sonner'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import {
  PromptInput,
  PromptInputBody,
  PromptInputSubmit,
  PromptInputTextarea,
  usePromptInputAttachments,
} from '../prompt-input'

function AttachmentCounter() {
  const { files } = usePromptInputAttachments()
  return <div data-testid='attachment-count'>{files.length}</div>
}

describe('PromptInput submit and conversion contracts', () => {
  const originalCreateObjectURL = URL.createObjectURL
  const originalRevokeObjectURL = URL.revokeObjectURL

  beforeEach(() => {
    URL.createObjectURL = vi.fn(() => 'blob:http://localhost/test-blob-1')
    URL.revokeObjectURL = vi.fn()
  })

  afterEach(() => {
    cleanup()
    if (originalCreateObjectURL) {
      URL.createObjectURL = originalCreateObjectURL
    } else {
      delete (URL as { createObjectURL?: unknown }).createObjectURL
    }
    if (originalRevokeObjectURL) {
      URL.revokeObjectURL = originalRevokeObjectURL
    } else {
      delete (URL as { revokeObjectURL?: unknown }).revokeObjectURL
    }
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  test('converts blob URLs to data URLs on successful submit and clears attachments', async () => {
    const onSubmit = vi.fn()
    const mockBlob = new Blob(['sample-file-content'], { type: 'text/plain' })

    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        blob: () => Promise.resolve(mockBlob),
      })
    )

    render(
      <PromptInput onSubmit={onSubmit}>
        <PromptInputBody>
          <PromptInputTextarea name='message' defaultValue='Hello world' />
          <AttachmentCounter />
        </PromptInputBody>
        <PromptInputSubmit />
      </PromptInput>
    )

    const fileInput = screen.getByLabelText('Upload files')
    const testFile = new File(['sample-file-content'], 'test.txt', {
      type: 'text/plain',
    })
    fireEvent.change(fileInput, { target: { files: [testFile] } })

    expect(screen.getByTestId('attachment-count')).toHaveTextContent('1')

    const submitButton = screen.getByRole('button', { name: 'Submit' })
    fireEvent.click(submitButton)

    await waitFor(() => {
      expect(onSubmit).toHaveBeenCalledTimes(1)
    })

    const submitCall = onSubmit.mock.calls[0][0]
    expect(submitCall.text).toBe('Hello world')
    expect(submitCall.files).toHaveLength(1)
    expect(submitCall.files[0].url).toMatch(/^data:text\/plain;base64,/)
    expect(submitCall.files[0].filename).toBe('test.txt')

    expect(screen.getByTestId('attachment-count')).toHaveTextContent('0')
  })

  test('handles blob conversion rejection with onError, retains attachments for retry, and does not invoke onSubmit', async () => {
    const onSubmit = vi.fn()
    const onError = vi.fn()

    vi.stubGlobal(
      'fetch',
      vi.fn().mockRejectedValue(new Error('Network failure downloading blob'))
    )

    render(
      <PromptInput onError={onError} onSubmit={onSubmit}>
        <PromptInputBody>
          <PromptInputTextarea name='message' defaultValue='Keep this' />
          <AttachmentCounter />
        </PromptInputBody>
        <PromptInputSubmit />
      </PromptInput>
    )

    const fileInput = screen.getByLabelText('Upload files')
    const testFile = new File(['failing-file'], 'failed.txt', {
      type: 'text/plain',
    })
    fireEvent.change(fileInput, { target: { files: [testFile] } })

    expect(screen.getByTestId('attachment-count')).toHaveTextContent('1')

    const submitButton = screen.getByRole('button', { name: 'Submit' })
    fireEvent.click(submitButton)

    await waitFor(() => {
      expect(onError).toHaveBeenCalledTimes(1)
    })

    expect(onError).toHaveBeenCalledWith(
      expect.objectContaining({
        code: 'conversion',
        message: expect.any(String),
      })
    )

    expect(onSubmit).not.toHaveBeenCalled()
    expect(screen.getByTestId('attachment-count')).toHaveTextContent('1')
  })

  test('reports conversion failure via toast.error when no onError callback is provided', async () => {
    const toastErrorSpy = vi.spyOn(toast, 'error').mockImplementation(() => '')
    const onSubmit = vi.fn()

    vi.stubGlobal(
      'fetch',
      vi.fn().mockRejectedValue(new Error('Conversion rejected'))
    )

    render(
      <PromptInput onSubmit={onSubmit}>
        <PromptInputBody>
          <PromptInputTextarea name='message' defaultValue='Toast on error' />
          <AttachmentCounter />
        </PromptInputBody>
        <PromptInputSubmit />
      </PromptInput>
    )

    const fileInput = screen.getByLabelText('Upload files')
    const testFile = new File(['test'], 'test.txt', { type: 'text/plain' })
    fireEvent.change(fileInput, { target: { files: [testFile] } })

    const submitButton = screen.getByRole('button', { name: 'Submit' })
    fireEvent.click(submitButton)

    await waitFor(() => {
      expect(toastErrorSpy).toHaveBeenCalledWith('Something went wrong!')
    })

    expect(onSubmit).not.toHaveBeenCalled()
    expect(screen.getByTestId('attachment-count')).toHaveTextContent('1')
  })

  test('does not overwrite user input entered during asynchronous conversion', async () => {
    const onSubmit = vi.fn()

    let resolveBlobFetch: (value: {
      blob: () => Promise<Blob>
    }) => void = () => {}
    const delayedFetch = new Promise<{ blob: () => Promise<Blob> }>(
      (resolve) => {
        resolveBlobFetch = resolve
      }
    )

    vi.stubGlobal('fetch', vi.fn().mockReturnValue(delayedFetch))

    render(
      <PromptInput onSubmit={onSubmit}>
        <PromptInputBody>
          <PromptInputTextarea name='message' />
          <AttachmentCounter />
        </PromptInputBody>
        <PromptInputSubmit />
      </PromptInput>
    )

    const fileInput = screen.getByLabelText('Upload files')
    const testFile = new File(['content'], 'file.txt', { type: 'text/plain' })
    fireEvent.change(fileInput, { target: { files: [testFile] } })

    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement
    fireEvent.change(textarea, { target: { value: 'Initial captured prompt' } })
    expect(textarea.value).toBe('Initial captured prompt')

    const submitButton = screen.getByRole('button', { name: 'Submit' })
    fireEvent.click(submitButton)

    expect(textarea.value).toBe('')

    fireEvent.change(textarea, {
      target: { value: 'New text typed while conversion in progress' },
    })
    expect(textarea.value).toBe('New text typed while conversion in progress')

    const mockBlob = new Blob(['content'], { type: 'text/plain' })
    resolveBlobFetch({ blob: () => Promise.resolve(mockBlob) })

    await waitFor(() => {
      expect(onSubmit).toHaveBeenCalledTimes(1)
    })

    expect(onSubmit.mock.calls[0][0].text).toBe('Initial captured prompt')
    expect(textarea.value).toBe('New text typed while conversion in progress')
  })
})
