import { getPayload } from 'payload'
import config from '../../payload.config.mjs'

export const dynamic = 'force-dynamic'
export async function GET() {
  await getPayload({ config })
  return Response.json({ ready: true })
}
