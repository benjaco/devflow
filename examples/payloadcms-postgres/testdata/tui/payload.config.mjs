import { buildConfig } from 'payload'
import { postgresAdapter } from '@payloadcms/db-postgres'
import { Posts } from './collections/Posts.mjs'

export default buildConfig({
  secret: 'devflow-disposable-tui-fixture',
  telemetry: false,
  db: postgresAdapter({
    pool: { connectionString: process.env.DATABASE_URL },
    push: process.env.PAYLOAD_SCHEMA_PUSH === 'true',
  }),
  collections: [Posts],
})
