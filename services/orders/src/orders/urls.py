from django.urls import path

from orders import views

urlpatterns = [
    path("quotes", views.QuoteCreateView.as_view(), name="quotes"),
    path("orders", views.OrderCreateView.as_view(), name="orders"),
    path("orders/<str:order_id>", views.OrderDetailView.as_view(), name="order-detail"),
    path("orders/<str:order_id>/cancel", views.OrderCancelView.as_view(), name="order-cancel"),
    path("courier/orders/<str:order_id>/pickup", views.PickupView.as_view(), name="courier-pickup"),
    path(
        "courier/orders/<str:order_id>/deliver",
        views.DeliverView.as_view(),
        name="courier-deliver",
    ),
    path("courier/orders/<str:order_id>/fail", views.FailView.as_view(), name="courier-fail"),
]
