# Fixture: false-positive guard cases

This file tests that the script does NOT emit tags for these patterns:

- RC pre-release version: v9.9.9-rc1 is a release candidate, not a tag claim
- RC artifact: criteria-v9.9.9-rc2 upload is a release artifact
- Version without a keyword: the changelog documents v9.7.0 features

Only this line should produce a claim: see the v9.6.0 tag.
