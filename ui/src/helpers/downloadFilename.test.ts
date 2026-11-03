import { describe, expect, it } from 'vitest'
import { downloadFilename } from './downloadFilename'

describe('downloadFilename', () => {
  it('preserves the server-provided original GPX filename', () => {
    expect(
      downloadFilename(
        "attachment; filename*=utf-8''Morning%20walk%20%C3%BC.GPX",
        '/api/photo/random.GPX'
      )
    ).toBe('Morning walk ü.GPX')
    expect(
      downloadFilename(
        'attachment; filename="Day walk.gpx"',
        '/api/photo/random.gpx'
      )
    ).toBe('Day walk.gpx')
  })
  it('accepts the unquoted filename tokens emitted by the Go download route', () => {
    expect(
      downloadFilename('attachment; filename=walk.gpx', '/api/photo/random.gpx')
    ).toBe('walk.gpx')
  })
  it('keeps existing URL filenames when there is no disposition', () => {
    expect(downloadFilename(null, '/api/photo/highres.jpg')).toBe('highres.jpg')
  })
  it('handles malformed header encoding and strips directory components', () => {
    expect(
      downloadFilename(
        "attachment; filename*=utf-8''%ZZ",
        '/api/photo/fallback.gpx'
      )
    ).toBe('fallback.gpx')
    expect(
      downloadFilename(
        'attachment; filename="../walk.gpx"',
        '/api/photo/random.gpx'
      )
    ).toBe('walk.gpx')
  })
})
