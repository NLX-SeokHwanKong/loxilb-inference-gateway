"""Generate a fresh fixture password satisfying the management API's policy."""
import re
import secrets


def generate_password():
    # CSPRNG entropy plus guaranteed upper/lower/digit/symbol categories.
    # Reject triple repetitions, including prefix/tail boundary, rather than
    # weakening the API policy or retrying account creation with a fixed secret.
    while True:
        value='Uc7!'+secrets.token_hex(24)
        if not re.search(r'(.)\1\1',value):
            return value


if __name__=='__main__':
    print(generate_password())
