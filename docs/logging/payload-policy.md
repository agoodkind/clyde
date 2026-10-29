# Payload Policy

Request and response legs log identity, path, outcome (status, byte counts, duration), and typed facets. The SQLite capture store saves full request and response bodies only during an enabled recording period. The [capture settings](config.md) control recording and retention.

MITM wire legs use the `providers.mitm.wire` concern. Recorded exchanges and wire legs share `request_id` and `trace_id` values. Adapter egress captures include the egress client label.
