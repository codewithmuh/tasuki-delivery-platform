from django.urls import path

from accounts import views

urlpatterns = [
    path("admin/merchants", views.MerchantCreateView.as_view(), name="admin-merchants"),
    path("admin/couriers", views.CourierCreateView.as_view(), name="admin-couriers"),
    path(
        "admin/couriers/<str:courier_id>/token",
        views.CourierTokenView.as_view(),
        name="admin-courier-token",
    ),
]
