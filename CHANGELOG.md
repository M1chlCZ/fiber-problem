# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `FieldErrors` validation error aggregation. It implements `error` with a
  deterministic `invalid fields: email, phone` message; `MergeFieldErrors`
  combines several sources and a later value wins per field.
- `FromError` maps a `FieldErrors` error to a 422 Unprocessable Entity problem
  with `field_errors` set. Wrapped errors match too; existing mappings are
  unchanged.
- `schema/problem.openapi.yaml`, an OpenAPI schema snippet for the
  `application/problem+json` response body.
