# Persea Agents Logger
# Add this import to your app's entry point (e.g., main.py or app.py):
#   import persea_logger
#
# This service runs outside GCP, so there is no Cloud Logging sink:
# it ships errors to the gateway over HTTP. Set these in the deployment
# environment (LOGCORE_KEY is a server secret — never commit it, and
# never give it a browser prefix):
#   LOGCORE_URL, LOGCORE_KEY, LOGCORE_ENV, LOGCORE_MIN_SEVERITY

import os

from ablock_logger import configure as configure_logcore

configure_logcore(
    service="golang-gin-realworld-example-app",
    env=os.environ.get("LOGCORE_ENV", "prod"),
    gateway_endpoint=os.environ.get("LOGCORE_URL"),
    gateway_api_key=os.environ.get("LOGCORE_KEY"),
    gateway_min_severity=os.environ.get("LOGCORE_MIN_SEVERITY", "ERROR"),
)
