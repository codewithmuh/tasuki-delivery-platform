from django.apps import AppConfig


class AccountsConfig(AppConfig):
    name = "accounts"

    def ready(self) -> None:
        # Importing registers the auth schemes with drf-spectacular.
        from accounts import schema  # noqa: F401
