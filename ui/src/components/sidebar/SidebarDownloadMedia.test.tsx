import React from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MockedProvider } from '@apollo/client/testing'
import SidebarMediaDownload from './SidebarDownloadMedia'
import { MediaType } from '../../__generated__/globalTypes'
import { clearTokenCookie, saveTokenCookie } from '../../helpers/authentication'

const createObjectURL = Object.getOwnPropertyDescriptor(URL, 'createObjectURL')
const revokeObjectURL = Object.getOwnPropertyDescriptor(URL, 'revokeObjectURL')
const originalFetch = globalThis.fetch
let downloadedBlob: Blob | undefined
let downloadedFilename: string
const payload = new Blob(['<gpx><trk/></gpx>'], { type: 'application/gpx+xml' })

function showDownload(url: string) {
  render(
    <MockedProvider>
      <SidebarMediaDownload
        media={{
          __typename: 'Media',
          id: '1',
          type: MediaType.Photo,
          downloads: [
            {
              __typename: 'MediaDownload',
              title: 'Original',
              mediaUrl: {
                __typename: 'MediaURL',
                url,
                width: 1024,
                height: 796,
                fileSize: payload.size,
              },
            },
          ],
        }}
      />
    </MockedProvider>
  )
  fireEvent.click(screen.getByText('Original'))
}

beforeEach(() => {
  downloadedBlob = undefined
  downloadedFilename = ''
  window.history.replaceState({}, '', '/')
  saveTokenCookie('synthetic-login')
  globalThis.fetch = vi.fn().mockResolvedValue({
    headers: new Headers({
      'Content-Type': 'application/gpx+xml',
      'Content-Disposition': 'attachment; filename="Synthetic walk.gpx"',
    }),
    blob: () => Promise.resolve(payload),
  })
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    value: (blob: Blob) => {
      downloadedBlob = blob
      return 'blob:synthetic-download'
    },
  })
  Object.defineProperty(URL, 'revokeObjectURL', {
    configurable: true,
    value: vi.fn(),
  })
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
    this: HTMLAnchorElement
  ) {
    downloadedFilename = this.download
  })
})

afterEach(() => {
  clearTokenCookie()
  window.history.replaceState({}, '', '/')
  globalThis.fetch = originalFetch
  if (createObjectURL)
    Object.defineProperty(URL, 'createObjectURL', createObjectURL)
  else Reflect.deleteProperty(URL, 'createObjectURL')
  if (revokeObjectURL)
    Object.defineProperty(URL, 'revokeObjectURL', revokeObjectURL)
  else Reflect.deleteProperty(URL, 'revokeObjectURL')
  vi.restoreAllMocks()
})

describe('media downloads', () => {
  it.each([
    'https://gallery.example.test/api/photo/DSC05922_test.JPG',
    'https://api.example.test/gallery/api/photo/track.GPX?download=1',
    'https://gallery.example.test/api/photo/folder%20name_track.GPX',
  ])('downloads an absolute API URL unchanged: %s', async url => {
    showDownload(url)
    await waitFor(() => expect(downloadedFilename).toBe('Synthetic walk.gpx'))
    expect(fetch).toHaveBeenCalledWith(url, { credentials: 'include' })
    expect(downloadedBlob).toBe(payload)
  })

  it('resolves a root-relative API URL independently of the current album page', async () => {
    window.history.replaceState({}, '', '/album/123')
    showDownload('/gallery/api/photo/track.GPX')
    await waitFor(() => expect(downloadedFilename).toBe('Synthetic walk.gpx'))
    expect(fetch).toHaveBeenCalledWith(
      `${location.origin}/gallery/api/photo/track.GPX`,
      { credentials: 'include' }
    )
  })

  it('preserves API query parameters and adds the shared album token', async () => {
    clearTokenCookie()
    window.history.replaceState({}, '', '/share/synthetic123/album/1')
    showDownload('https://api.example.test/api/photo/track.GPX?download=1')
    await waitFor(() => expect(downloadedFilename).toBe('Synthetic walk.gpx'))
    expect(fetch).toHaveBeenCalledWith(
      'https://api.example.test/api/photo/track.GPX?download=1&token=synthetic123',
      { credentials: 'include' }
    )
  })
})
