# Observability Assessment and Stack Guidelines for Hive

## Overview

This document gives guidelines for telemetry and observability in `hive`, the central orchestration plane for `tuna-os` infrastructure.

## Current Telemetry Stack Assessment

1. **Backend Status**: Operators have not configured an external telemetry backend. We add no off-box exporters without explicit operator configuration.
2. **Architecture**: `hive` contains daemon binaries and CLI tools. Diagnostics depend on standard output logs and exit codes.
3. **Observability Scope**: Primary requirements include operational metrics and diagnostic logs.

## Observability Guidelines & Target Architecture

- **Structured Logging**: Services and CLI commands should adopt JSON logs to support automated log ingestion.
- **Bounded Metrics Endpoint**: When operators configure a collector, expose metrics through a `/metrics` Prometheus endpoint.
- **Metric Cardinality Bounds**: Keep all metric dimensions bounded. Do not include agent session IDs or raw payloads in metric labels.
- **Span Context Propagation**: Execution across daemons and sub-agents should support propagation of OpenTelemetry trace context.
- **Data Protection & Compliance**: Telemetry data, traces, and log messages must never contain access tokens or credentials.
