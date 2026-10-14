"""Public routes. /healthz, /readyz and /metrics are answered by core.middleware, before routing."""

from django.urls import include, path
from drf_spectacular.views import SpectacularAPIView

urlpatterns = [
    path("v1/", include("accounts.urls")),
    path("v1/", include("orders.urls")),
    path("v1/openapi.yaml", SpectacularAPIView.as_view(), name="openapi"),
]

# Django's own error pages are HTML; the API always answers in its JSON error format.
handler400 = "core.errors.bad_request"
handler403 = "core.errors.permission_denied"
handler404 = "core.errors.not_found"
handler500 = "core.errors.server_error"
