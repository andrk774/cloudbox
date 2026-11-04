import 'package:cloudbox/api.dart';
import 'package:cloudbox/main.dart';
import 'package:cloudbox/screens/login.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  testWidgets('вход: пустые поля подсвечиваются, ошибка сервера показывается', (t) async {
    String? tried;
    await t.pumpWidget(CloudBoxApp(home: LoginScreen(connect: (s, u, p) async {
      tried = '$s|$u|$p';
      throw ApiException(401, 'Неверное имя пользователя или пароль приложения');
    })));
    await t.tap(find.byKey(const Key('login')));
    await t.pump();
    expect(find.text('Укажите адрес сервера'), findsOneWidget);
    expect(tried, isNull);

    await t.enterText(find.byKey(const Key('server')), 'http://192.168.1.5:8080');
    await t.pump();
    expect(find.textContaining('Без HTTPS'), findsOneWidget, reason: 'предупреждение о входе без шифрования');
    await t.enterText(find.byKey(const Key('user')), 'аня');
    await t.enterText(find.byKey(const Key('password')), ' abcde-12345 ');
    await t.ensureVisible(find.byKey(const Key('login'))); // предупреждение о HTTP сдвигает кнопку вниз
    await t.tap(find.byKey(const Key('login')));
    await t.pumpAndSettle();
    expect(tried, 'http://192.168.1.5:8080|аня|abcde-12345');
    expect(find.byKey(const Key('error')), findsOneWidget);
    expect(find.text('Неверное имя пользователя или пароль приложения'), findsOneWidget);
  });
}
