package main

func classifyBackgroundLaunchLockResult(
	locked bool, err error,
) (bool, error) {
	if err != nil && backgroundLaunchLockErrorMeansContention(err) {
		return false, nil
	}
	return locked, err
}
