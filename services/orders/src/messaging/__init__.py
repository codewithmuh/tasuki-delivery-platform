"""Kafka in and out, with database guarantees: the transactional outbox and the idempotent inbox.

This package knows nothing about orders. The orders app decides what to publish and how to react.
"""
