// Prefer the original filename supplied by authenticated download responses.
export function downloadFilename(
  disposition: string | null,
  url: string
): string {
  const fallback = url.match(/[^/]*$/)?.[0] || 'download'
  const encoded = disposition?.match(/filename\*=utf-8''([^;]+)/i)
  const quoted = disposition?.match(/filename="((?:\\.|[^"\\])*)"/i)
  const token = disposition?.match(/filename=([^;\s]+)/i)
  let filename = fallback
  if (encoded) {
    try {
      filename = decodeURIComponent(encoded[1])
    } catch {
      return fallback
    }
  } else if (quoted) {
    filename = quoted[1].replace(/\\(.)/g, '$1')
  } else if (token) {
    filename = token[1]
  }
  return filename.split(/[/\\]/).pop() || fallback
}
