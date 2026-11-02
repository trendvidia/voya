- **`client/reflection` serves nothing on Fly** ([#332](https://github.com/trendvidia/voya/issues/332)).
  `Mount` and `MountFrom` mounted their handlers unconditionally, so a service
  following README published its full schema in production. When `FLY_APP_NAME`
  is set they now mount nothing and return nil. A service that wants reflection
  on Fly mounts grpcreflect's handlers itself. README also now states the rule
  for a production host that is not Fly: set `FLY_APP_NAME`, or do not call
  `Join`.
