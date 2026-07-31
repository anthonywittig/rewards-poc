import { errorCode, errorMessage } from '../format'

export function ErrorBanner({ error }: { error: unknown }) {
  if (!error) return null
  const code = errorCode(error)
  return (
    <div className="error-banner" role="alert">
      {code ? (
        <>
          <code>{code}</code>
          {' — '}
        </>
      ) : null}
      {errorMessage(error)}
    </div>
  )
}
