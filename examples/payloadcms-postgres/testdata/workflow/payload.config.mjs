import { buildConfig } from 'payload'
import { postgresAdapter } from '@payloadcms/db-postgres'
import { readFileSync } from 'node:fs'
import path from 'node:path'

const shape = JSON.parse(readFileSync(path.resolve('schema.json'), 'utf8'))
export default buildConfig({
  secret: 'devflow-disposable-workflow-fixture',
  telemetry: false,
  db: postgresAdapter({
    pool: { connectionString: process.env.DATABASE_URL },
    push: process.env.PAYLOAD_SCHEMA_PUSH === 'true',
    migrationDir: path.resolve('migrations'),
  }),
  collections: [{ slug: 'posts', fields: [
    { name: shape.title, type: 'text' },
    ...(shape.legacy ? [{ name: 'legacy', type: 'text' }] : []),
    { name: 'layout', type: 'blocks', blocks: [{ slug: shape.block, fields: [
      { name: shape.caption, type: 'text' },
    ] }] },
  ] }],
})
